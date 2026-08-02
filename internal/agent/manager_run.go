package agent

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/buildcache"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/notes"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/task"
	"github.com/google/uuid"
)

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt, dir string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

// RunWithCapacityReservation consumes reservation if the run reaches
// registerRunningAgent; otherwise the caller still owns it and must release it.
func (m *Manager) RunWithCapacityReservation(cfg RunConfig, reservation *CapacityReservation) (*Agent, error) {
	cfg.capacityReservation = reservation
	return m.Run(cfg)
}

func (a *Agent) setAssignment(cfg RunConfig) {
	a.ExperimentID = cfg.ExperimentID
	a.VariantID = cfg.VariantID
	a.RoutingReason = cfg.RoutingReason
	a.AssignmentUnit = cfg.AssignmentUnit
	a.AssignmentKey = cfg.AssignmentKey
	a.DecisionVersion = cfg.DecisionVersion
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	return m.RunContext(m.ctx, cfg)
}

func (m *Manager) RunContext(ctx context.Context, cfg RunConfig) (*Agent, error) {
	if cfg.Mode == "headless" {
		if err := m.jitterDispatchContext(ctx); err != nil {
			return nil, err
		}
	}

	cfg, prov, err := m.prepareRunConfig(cfg) //nolint:contextcheck // provider gating emits via manager-owned app lifecycle, not per-run ctx
	if err != nil {
		return nil, err
	}

	id := uuid.NewString()[:8]
	ctx, cancel := context.WithCancel(ctx)
	a := newRunningAgent(id, cfg, prov, cancel)
	cfg = injectProcessOwnerEnv(cfg, processOwnerForAgent(a))
	if m.survives() && willDetach(cfg) {
		a.setDetached(true)
	}

	if err := m.registerRunningAgent(a, cfg, cancel); err != nil { //nolint:contextcheck // per-class metrics are process-global accounting, not tied to per-run ctx
		return nil, err
	}

	metrics.AgentStarted(a.Provider, a.Mode) //nolint:contextcheck // metrics are process-global accounting, not tied to per-run ctx
	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "provider", a.Provider, "model", a.Model)

	if err := m.startAgentRunner(ctx, a, cfg, prov, cancel); err != nil {
		return nil, err
	}

	m.emit(events.AgentState(id), a)
	return a, nil
}

// jitterDispatch sleeps a uniform random [0, dispatchJitterMs] duration so a
// wave of ready headless dispatches does not all probe the provider health
// gate in the same tick. Returns the context error if the manager shuts down
// mid-sleep, so the caller aborts the dispatch instead of racing shutdown.
func (m *Manager) jitterDispatch() error {
	return m.jitterDispatchContext(m.ctx)
}

func (m *Manager) jitterDispatchContext(ctx context.Context) error {
	m.mu.RLock()
	ms := m.dispatchJitterMs
	m.mu.RUnlock()
	if ms <= 0 {
		return nil
	}
	// Exclusive upper bound (ms, not ms+1) avoids overflow when ms == MaxInt
	// while keeping the jitter within [0, ms).
	d := time.Duration(rand.N(ms)) * time.Millisecond
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// warnUnenforceableAllowedTools reports a step whose allowed_tools list the
// resolved provider cannot enforce.
//
// The check lives here rather than in Definition.Validate because `ab` and
// `cross` pick the provider at dispatch: the same step is enforced on a claude
// spawn and unenforced on the copilot spawn beside it, so which guarantee you
// get depends on where the run landed. Nothing surfaced that before.
//
// It warns rather than refusing or re-routing. Excluding providers that cannot
// enforce would silently narrow failover and strand a step whenever claude is
// capped — the stranding shape that produced #2150's spin loop — and refusing
// outright would break every step that has always used the list as an allowance
// rather than a fence. The OS-level process sandbox (procsandbox_*.go) is the
// boundary that actually binds every provider; allowed_tools is advisory
// wherever this warns.
func (m *Manager) warnUnenforceableAllowedTools(cfg RunConfig, prov Provider) {
	if len(cfg.AllowedTools) == 0 || prov.HonorsAllowedTools() {
		return
	}
	m.logger.Warn("agent.run.allowed_tools.unenforced",
		"task_id", cfg.TaskID, "name", cfg.Name, "provider", prov.Name(),
		"allowed_tools", strings.Join(cfg.AllowedTools, ","),
		"detail", "provider ignores allowed_tools; the agent runs with its default tool reach and only the prompt and the process sandbox constrain it")
}

func (m *Manager) prepareRunConfig(cfg RunConfig) (RunConfig, Provider, error) {
	if err := validateRunDir(cfg.Dir); err != nil {
		return cfg, nil, err
	}
	role, roleErr := ResolveRunRole(cfg.Role, cfg.Name)
	if roleErr != nil {
		return cfg, nil, roleErr
	}
	cfg.Role = role
	m.mu.RLock()
	if cfg.Model == "" {
		cfg.Model = m.defaultModel
	}
	m.mu.RUnlock()
	if cfg.SeedWorkingMemory {
		cfg.Prompt = notes.SeedPrompt(cfg.Prompt, cfg.Dir)
	}
	cfg.Prompt = withBackgroundTaskGuardrail(cfg.Prompt, cfg)
	resolvedProvider, routingReason, gateEvents, gateErr := m.resolveProviderDecision(cfg)
	if gateErr != nil {
		m.emitProviderGateEvents(gateEvents)
		return cfg, nil, gateErr
	}
	m.emitProviderGateEvents(gateEvents)
	cfg.RoutingReason = routingReason
	prov, providerErr := lookupProvider(resolvedProvider)
	if providerErr != nil {
		return cfg, nil, providerErr
	}
	requestedProvider, requestedErr := m.providerForRun(cfg.Provider)
	if requestedErr != nil {
		return cfg, nil, requestedErr
	}
	resolvedModel, nextRequestedModel, modelErr := resolveRunModel(requestedProvider, prov.Name(), cfg.Model)
	if modelErr != nil {
		m.logger.Warn("agent.run.provider_model_incompatible",
			"task_id", cfg.TaskID,
			"requested_provider", requestedProvider,
			"selected_provider", prov.Name(),
			"model", cfg.Model,
			"err", modelErr,
		)
		return cfg, nil, modelErr
	}
	cfg.Model = nextRequestedModel
	cfg.resolvedModel = resolvedModel
	if err := m.resolveWorkflowSkillPrompt(&cfg, prov); err != nil {
		return cfg, nil, err
	}
	cfg.provider = prov
	m.warnUnenforceableAllowedTools(cfg, prov)
	cfg.ReasoningEffort = m.resolveReasoningEffort(cfg.Role, cfg.ReasoningEffort)
	if cfg.Mode == "headless" {
		m.mu.RLock()
		steerable := m.headlessSteerable
		m.mu.RUnlock()
		// Verifier/system roles and fix-review dispatch unattended (no caller
		// ever writes a steer message) — see Role.SupportsHeadlessSteer.
		cfg.HeadlessSteerable = steerable && cfg.Role.SupportsHeadlessSteer()
	}
	cfg.approvalAddr = m.approvalAddr
	// Headless Claude runs with require_permissions:true rely on Sybra's
	// approval hook to gate each tool call. If the approval server never
	// started (approvalAddr empty) the hook is silently omitted and the run
	// falls back to CLI defaults — neither the gating the operator asked for
	// nor an explicit bypass. Fail closed rather than degrading quietly.
	//
	// Scope this to the exact vulnerable shape:
	// - claude provider
	// - headless mode
	// - no explicit AllowedTools allowlist
	// - not using Claude's own auto classifier
	//
	// Other providers do not depend on this hook for headless execution.
	if prov.Name() == "claude" && cfg.Mode == "headless" &&
		cfg.RequirePermissions && cfg.approvalAddr == "" &&
		len(cfg.AllowedTools) == 0 && cfg.HeadlessPermissionMode != "auto" {
		return cfg, nil, fmt.Errorf("require_permissions requires a running approval server for ungated headless claude runs")
	}

	if err := m.injectSandboxHome(&cfg); err != nil {
		return cfg, nil, err
	}

	m.injectGhShim(&cfg)

	if err := m.injectGolangciCache(&cfg); err != nil {
		return cfg, nil, err
	}

	if err := m.injectSharedBuildCache(&cfg); err != nil {
		return cfg, nil, err
	}

	m.injectGitHubToken(&cfg)

	if err := m.injectProcessSandbox(&cfg); err != nil {
		return cfg, nil, err
	}

	m.preparePlaywrightMCP(&cfg)

	m.mu.RLock()
	if cfg.BashTimeoutMs == 0 {
		cfg.BashTimeoutMs = m.bashTimeoutMs
	}
	if cfg.RetryWatchdog == 0 {
		cfg.RetryWatchdog = m.retryWatchdog
	}
	if cfg.FallbackModel == "" {
		cfg.FallbackModel = m.fallbackModel
	}
	m.mu.RUnlock()
	return cfg, prov, nil
}

// injectSandboxHome routes every task-scoped agent subprocess's default
// SYBRA_HOME through the per-task sandbox home, so no fresh agent (any
// provider, any role) can resolve the operator's real ~/.sybra by default —
// see #1576. Taskless system runs skip this only when IsolateHome is false;
// long-lived/judgment agents such as the orchestrator opt in so stale Sybra
// source checkouts or sybra-cli invocations cannot rewrite the operator config.
//
// cfg.ExtraEnv is normalized before the trusted values are appended: any
// existing SYBRA_HOME/SYBRA_CONTROL_HOME entries (caller-supplied or
// otherwise) are stripped first, then the sandbox home and (if configured) the
// control home are appended last, so they always win regardless of duplicate
// env var resolution order in the target process.
func (m *Manager) injectSandboxHome(cfg *RunConfig) error {
	sandboxKey := strings.TrimSpace(cfg.TaskID)
	if sandboxKey == "" {
		if !cfg.IsolateHome {
			return nil
		}
		sandboxKey = systemSandboxKey(cfg)
	}
	cfg.sandboxKey = sandboxKey

	m.mu.RLock()
	resolve := m.sandboxHome
	controlHome := m.controlHome
	m.mu.RUnlock()
	if resolve == nil {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "sandbox_key", sandboxKey, "err", "no sandbox home resolver configured")
		return fmt.Errorf("agent.Run: no sandbox home resolver configured for run %q", sandboxKey)
	}
	dir, err := resolve(sandboxKey)
	if err != nil {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "sandbox_key", sandboxKey, "err", err)
		return fmt.Errorf("agent.Run: resolve sandbox home for run %q: %w", sandboxKey, err)
	}
	if strings.TrimSpace(dir) == "" {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "sandbox_key", sandboxKey, "err", "resolver returned empty path")
		return fmt.Errorf("agent.Run: sandbox home resolver returned empty path for run %q", sandboxKey)
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = fmt.Errorf("%q is not a directory", dir)
		}
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "sandbox_key", sandboxKey, "err", statErr)
		return fmt.Errorf("agent.Run: sandbox home %q for run %q is not accessible: %w", dir, sandboxKey, statErr)
	}

	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "SYBRA_HOME", "SYBRA_CONTROL_HOME")
	cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_HOME="+dir)
	if controlHome != "" {
		cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_CONTROL_HOME="+controlHome)
	}
	cfg.resolvedSandboxHome = dir
	return nil
}

func systemSandboxKey(cfg *RunConfig) string {
	raw := strings.TrimSpace(cfg.Name)
	if raw == "" {
		raw = strings.TrimSpace(string(cfg.Role))
	}
	if raw == "" {
		return "system-run"
	}
	raw = strings.ToLower(raw)
	var b strings.Builder
	b.Grow(len("system-") + len(raw))
	b.WriteString("system-")
	lastDash := false
	for _, r := range raw {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-'
		if !ok {
			if lastDash {
				continue
			}
			r = '-'
		}
		if r == '-' {
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	key := strings.Trim(b.String(), "-.")
	if key == "system" || key == "" {
		return "system-run"
	}
	return key
}

func (m *Manager) injectGitHubToken(cfg *RunConfig) {
	m.mu.RLock()
	tokenFn := m.ghAppToken
	m.mu.RUnlock()
	if tokenFn == nil {
		return
	}
	token := tokenFn()
	if token == "" {
		return
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "GH_TOKEN", "GITHUB_TOKEN")
	// Do not snapshot the short-lived App token into the provider process env:
	// agents can run longer than GitHub installation tokens live, and a stale
	// GH_TOKEN wins over the credential helper even after Sybra refreshes its
	// cache. Override any ambient token to empty here; the PATH-level gh shim
	// mints a fresh token for each short-lived gh child process instead.
	cfg.ExtraEnv = append(cfg.ExtraEnv, "GH_TOKEN=", "GITHUB_TOKEN=")
}

func (m *Manager) injectGolangciCache(cfg *RunConfig) error {
	if cfg.resolvedSandboxHome == "" {
		return nil
	}
	dir := filepath.Join(cfg.resolvedSandboxHome, "golangci-lint-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("agent.Run: create golangci-lint cache for task %q: %w", cfg.TaskID, err)
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "GOLANGCI_LINT_CACHE")
	cfg.ExtraEnv = append(cfg.ExtraEnv, "GOLANGCI_LINT_CACHE="+dir)
	return nil
}

func sharedBuildCacheDir() string {
	return buildcache.SharedRoot()
}

func (m *Manager) injectSharedBuildCache(cfg *RunConfig) error {
	if cfg.resolvedSandboxHome == "" {
		return nil
	}
	cacheKey := cfg.TaskID
	if cacheKey == "" {
		cacheKey = cfg.sandboxKey
	}
	goBuild := buildcache.TaskGoBuildDir(cacheKey)
	goMod := buildcache.SharedGoModDir()
	npm := buildcache.SharedNPMDir()
	for _, d := range []string{goBuild, goMod, npm} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("agent.Run: create shared build cache %q: %w", d, err)
		}
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "GOCACHE", "GOMODCACHE", "npm_config_cache")
	cfg.ExtraEnv = append(cfg.ExtraEnv,
		"GOCACHE="+goBuild,
		"GOMODCACHE="+goMod,
		"npm_config_cache="+npm,
	)
	return nil
}

// injectProcessSandbox resolves this run's OS-level process-sandbox posture
// and allowed write roots (worktree, per-task sandbox home, tmp, task-scoped
// git metadata) into
// cfg.sandbox, applied later by wrapInvocation at each provider spawn site.
//
// enforce fails closed — mirroring injectSandboxHome's discipline — when
// sandbox-exec is unavailable, the embedded profile cannot be materialized,
// or a root cannot be canonicalized: the error aborts the run before any
// subprocess spawns. report never blocks: the same failures are logged and
// this run's spec falls back to "off" (unwrapped) instead of erroring, so a
// misconfigured or unsupported host cannot break the default posture.
func (m *Manager) injectProcessSandbox(cfg *RunConfig) error {
	requested := cfg.SandboxMode
	if strings.TrimSpace(requested) == "" {
		m.mu.RLock()
		requested = m.defaultSandboxMode
		m.mu.RUnlock()
	}
	mode, err := config.NormalizeSandboxMode(requested)
	if err != nil {
		return fmt.Errorf("agent.Run: sandbox mode: %w", err)
	}
	cfg.SandboxMode = mode
	if mode == "off" {
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}
	if cfg.ReadOnlyDir {
		return m.injectReadOnlyProcessSandbox(cfg, mode)
	}
	worktree := cfg.Dir
	sandboxHome := cfg.resolvedSandboxHome
	if sandboxHome == "" {
		sandboxHome = worktree
	}
	tmp := os.TempDir()
	sharedCache := sharedBuildCacheDir()
	gitCtx := context.Background()
	if m.ctx != nil {
		gitCtx = context.WithoutCancel(m.ctx)
	}
	gitRoots, gitErr := resolveGitSandboxRoots(gitCtx, worktree)

	if !sandboxExecAvailable() {
		if mode == "enforce" {
			err := fmt.Errorf("agent.Run: enforce sandbox mode requires %s, which is unavailable on this host", sandboxWrapperName())
			m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
			return err
		}
		m.logger.Warn("agent.sandbox.report.unavailable", "task_id", cfg.TaskID)
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}
	if mode != "enforce" {
		if gitErr != nil {
			m.logger.Warn("agent.sandbox.report.git_roots_failed", "task_id", cfg.TaskID, "err", gitErr)
		}
		m.logProcessSandboxReport(cfg.TaskID, worktree, sandboxHome, tmp, sharedCache, gitRoots)
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}
	return m.buildEnforceSpec(cfg, gitCtx, worktree, sandboxHome, tmp, sharedCache, gitRoots, gitErr)
}

// buildEnforceSpec canonicalizes every enforce-mode write root and the git
// branch overlay, then assembles cfg.sandbox. Split out of
// injectProcessSandbox purely to keep that function's posture-selection
// logic (off/report/enforce, sandbox-exec availability) readable on its own.
func (m *Manager) buildEnforceSpec(cfg *RunConfig, gitCtx context.Context, worktree, sandboxHome, tmp, sharedCache string, gitRoots gitSandboxRoots, gitErr error) error {
	canonWorktree, err := canonicalizeRoot(worktree)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox worktree root: %w", err)
	}
	gitMetadata := m.resolveGitMetadataRoots(cfg.TaskID, canonWorktree)
	canonSandboxHome, err := canonicalizeRoot(sandboxHome)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox home root: %w", err)
	}
	canonTmp, err := canonicalizeRoot(tmp)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox tmp root: %w", err)
	}
	canonSharedCache, err := canonicalizeCreatedRoot(sharedCache, 0o755)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox shared-cache root: %w", err)
	}
	if gitErr != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", gitErr)
		return fmt.Errorf("agent.Run: sandbox git metadata roots: %w", gitErr)
	}
	gitOverlay, err := prepareGitSandboxOverlay(gitCtx, worktree, canonSandboxHome, gitRoots)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox git branch overlay: %w", err)
	}
	if err := injectSandboxGitEnv(cfg, gitRoots, gitOverlay); err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox git object env: %w", err)
	}
	profilePath, err := materializeSandboxProfile()
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox profile: %w", err)
	}
	cfg.sandbox = enforceSpec(canonWorktree, gitMetadata, canonSandboxHome, canonTmp, canonSharedCache, profilePath, "", gitRoots, gitOverlay)
	m.logger.Info("agent.sandbox.enforce", "task_id", cfg.TaskID,
		"worktree", canonWorktree, "sandbox_home", canonSandboxHome, "tmp", canonTmp,
		"git_metadata", cfg.sandbox.gitMetadata,
		"git_shared", cfg.sandbox.gitShared,
		"git_readonly", cfg.sandbox.gitReadonly,
		"shared_cache", canonSharedCache, "claude_state", cfg.sandbox.claudeState,
		"codex_state", cfg.sandbox.codexState, "copilot_state", cfg.sandbox.copilotState,
		"opencode_state", cfg.sandbox.opencodeState, "git_admin", cfg.sandbox.gitAdminDir,
		"git_common", cfg.sandbox.gitCommonDir, "git_worktrees", cfg.sandbox.gitWorktrees,
		"tool_cache", cfg.sandbox.toolCache, "profile", profilePath)
	return nil
}

// injectReadOnlyProcessSandbox is injectProcessSandbox's variant for runs
// whose cfg.Dir must stay read-only under enforce (RunConfig.ReadOnlyDir).
// It grants the same sandbox-home/tmp/shared-cache write roots as the normal
// path but never registers cfg.Dir, its .git metadata, or a git overlay as
// one of those writable roots.
//
// That omission alone is not sufficient: tmp/sandboxHome/sharedCache are
// broad roots (tmp in particular is the whole system temp dir) that can
// legitimately contain cfg.Dir as a subdirectory — e.g. a manual-test rig
// that stages every path under one /tmp/tmp.XXXXXX sandbox — and a bind
// mount only shadows an ancestor's mount for paths bound *after* it. So
// cfg.Dir is additionally canonicalized into sandbox.readOnlyDir and
// re-bound read-only as the very last bind in wrapInvocation, overriding any
// such accidental containment inside one of the writable roots.
func (m *Manager) injectReadOnlyProcessSandbox(cfg *RunConfig, mode string) error {
	dir := cfg.Dir
	sandboxHome := cfg.resolvedSandboxHome
	tmp := os.TempDir()
	sharedCache := sharedBuildCacheDir()

	if !sandboxExecAvailable() {
		if mode == "enforce" {
			err := fmt.Errorf("agent.Run: enforce sandbox mode requires %s, which is unavailable on this host", sandboxWrapperName())
			m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
			return err
		}
		m.logger.Warn("agent.sandbox.report.unavailable", "task_id", cfg.TaskID)
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}
	if mode != "enforce" {
		m.logger.Info("agent.sandbox.report.readonly_dir", "task_id", cfg.TaskID, "dir", dir,
			"sandbox_home", m.reportSandboxRoot(cfg.TaskID, "sandbox_home", sandboxHome),
			"tmp", m.reportSandboxRoot(cfg.TaskID, "tmp", tmp),
			"shared_cache", m.reportSandboxRoot(cfg.TaskID, "shared_cache", sharedCache))
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}
	// sandboxHome must never fall back to dir here (unlike injectSandboxHome's
	// general fallback elsewhere): dir is exactly the path this function
	// exists to keep read-only, so defaulting sandboxHome to it would grant
	// the write access ReadOnlyDir was set to deny.
	if sandboxHome == "" {
		err := fmt.Errorf("agent.Run: read-only-dir run %q has no resolved sandbox home", cfg.TaskID)
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return err
	}
	canonDir, err := canonicalizeRoot(dir)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox read-only dir: %w", err)
	}
	canonSandboxHome, err := canonicalizeRoot(sandboxHome)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox home root: %w", err)
	}
	canonTmp, err := canonicalizeRoot(tmp)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox tmp root: %w", err)
	}
	canonSharedCache, err := canonicalizeCreatedRoot(sharedCache, 0o755)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox shared-cache root: %w", err)
	}
	if canonDir == canonSandboxHome || canonDir == canonTmp || canonDir == canonSharedCache {
		err := fmt.Errorf("agent.Run: read-only dir %q collides with a writable sandbox root", canonDir)
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return err
	}
	profilePath, err := materializeSandboxProfile()
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox profile: %w", err)
	}
	cfg.sandbox = enforceSpec("", nil, canonSandboxHome, canonTmp, canonSharedCache, profilePath, canonDir, gitSandboxRoots{}, gitSandboxOverlay{})
	m.logger.Info("agent.sandbox.enforce.readonly_dir", "task_id", cfg.TaskID,
		"dir", canonDir, "sandbox_home", canonSandboxHome, "tmp", canonTmp,
		"shared_cache", canonSharedCache, "claude_state", cfg.sandbox.claudeState,
		"codex_state", cfg.sandbox.codexState, "copilot_state", cfg.sandbox.copilotState,
		"opencode_state", cfg.sandbox.opencodeState, "tool_cache", cfg.sandbox.toolCache,
		"profile", profilePath)
	return nil
}

func (m *Manager) logProcessSandboxReport(taskID, worktree, sandboxHome, tmp, sharedCache string, gitRoots gitSandboxRoots) {
	// report logs the resolved allowlist without wrapping the spawn, so the
	// provider CLI's would-be write footprint is visible before enforce rollout.
	logWorktree := m.reportSandboxRoot(taskID, "worktree", worktree)
	logSandboxHome := m.reportSandboxRoot(taskID, "sandbox_home", sandboxHome)
	logTmp := m.reportSandboxRoot(taskID, "tmp", tmp)
	logShared := m.reportSandboxRoot(taskID, "shared_cache", sharedCache)
	m.logger.Info("agent.sandbox.report", "task_id", taskID,
		"worktree", logWorktree, "sandbox_home", logSandboxHome, "tmp", logTmp, "shared_cache", logShared,
		"git_admin", gitRoots.adminDir, "git_common", gitRoots.commonDir, "git_worktrees", gitRoots.worktreesDir)
}

func (m *Manager) reportSandboxRoot(taskID, name, root string) string {
	canon, err := canonicalizeRoot(root)
	if err == nil {
		return canon
	}
	m.logger.Warn("agent.sandbox.report.canonicalize_failed", "task_id", taskID, "root", name, "err", err)
	return root
}

func canonicalizeCreatedRoot(root string, perm os.FileMode) (string, error) {
	if err := os.MkdirAll(root, perm); err != nil {
		return "", fmt.Errorf("create %s: %w", root, err)
	}
	return canonicalizeRoot(root)
}

func enforceSpec(
	worktree string,
	gitMetadata []string,
	sandboxHome, tmp, sharedCache, profilePath, readOnlyDir string,
	gitRoots gitSandboxRoots,
	gitOverlay gitSandboxOverlay,
) sandboxSpec {
	return sandboxSpec{
		mode:                   "enforce",
		worktree:               worktree,
		gitMetadata:            slices.Clone(gitMetadata),
		gitShared:              slices.Clone(gitRoots.sharedWritable),
		gitReadonly:            slices.Clone(gitRoots.sharedReadonly),
		sandboxHome:            sandboxHome,
		tmp:                    tmp,
		sharedCache:            sharedCache,
		profilePath:            profilePath,
		readOnlyDir:            readOnlyDir,
		gitAdminDir:            gitRoots.adminDir,
		gitCommonDir:           gitRoots.commonDir,
		gitWorktrees:           gitRoots.worktreesDir,
		gitObjectDir:           gitRoots.objectDir,
		gitBranchRef:           gitRoots.branchRef,
		gitBranchRefDir:        gitRoots.branchRefDir,
		gitBranchLogDir:        gitRoots.branchLogDir,
		gitRemoteRefDir:        gitRoots.remoteRefDir,
		gitRemoteLogDir:        gitRoots.remoteLogDir,
		gitTagRefDir:           gitRoots.tagRefDir,
		gitTagLogDir:           gitRoots.tagLogDir,
		gitOverlayObjectDir:    gitOverlay.objectDir,
		gitOverlayRefDir:       gitOverlay.branchRefDir,
		gitOverlayLogDir:       gitOverlay.branchLogDir,
		gitOverlayRefFile:      gitOverlay.branchRefFile,
		gitOverlayRemoteRefDir: gitOverlay.remoteRefDir,
		gitOverlayRemoteLogDir: gitOverlay.remoteLogDir,
		gitOverlayTagRefDir:    gitOverlay.tagRefDir,
		gitOverlayTagLogDir:    gitOverlay.tagLogDir,
		// Only projects/ — not the whole state dir. Sealing the root closes
		// settings.json, whose PreToolUse hooks would otherwise execute in
		// every later run, including verifier roles, and survive worktree
		// cleanup. projects/ has to stay writable because --resume reads the
		// session transcript from there: measured on the server, a fully
		// read-only ~/.claude still completes a run but fails resume with
		// "No conversation found with session ID" (#2779).
		claudeState:   agentStateRoot(".claude", sandboxHome),
		stateDenied:   claudeDurableConfigPaths(agentStateRoot(".claude", sandboxHome)),
		codexState:    agentStateRoot(".codex", sandboxHome),
		copilotState:  agentStateRoot(".copilot", sandboxHome),
		opencodeState: agentStateRoot(filepath.Join(".local", "share", "opencode"), sandboxHome),
		toolCache:     agentStateRoot(".cache", sandboxHome),
	}
}

func injectSandboxGitEnv(cfg *RunConfig, roots gitSandboxRoots, overlay gitSandboxOverlay) error {
	if roots.objectDir == "" && overlay.objectDir == "" {
		return nil
	}
	if roots.objectDir == "" || overlay.objectDir == "" {
		return fmt.Errorf("incomplete sandbox git object paths: shared=%q overlay=%q", roots.objectDir, overlay.objectDir)
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES")
	cfg.ExtraEnv = append(cfg.ExtraEnv,
		"GIT_OBJECT_DIRECTORY="+overlay.objectDir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+roots.objectDir,
	)
	return nil
}

func (m *Manager) resolveGitMetadataRoots(taskID, worktree string) []string {
	roots, err := gitMetadataRoots(worktree)
	if err != nil {
		m.logger.Warn("agent.sandbox.git-metadata", "task_id", taskID, "err", err)
		return nil
	}
	return roots
}

func gitMetadataRoots(worktree string) ([]string, error) {
	gitPath := filepath.Join(worktree, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat .git: %w", err)
	}
	if info.IsDir() {
		canon, err := canonicalizeRoot(gitPath)
		if err != nil {
			return nil, err
		}
		return []string{canon}, nil
	}

	data, err := os.ReadFile(gitPath)
	if err != nil {
		return nil, fmt.Errorf("read .git: %w", err)
	}
	firstLine, _, _ := strings.Cut(string(data), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(firstLine), "gitdir:")
	if !ok {
		return nil, fmt.Errorf("unsupported .git file first line %q", strings.TrimSpace(firstLine))
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	canonGitDir, err := canonicalizeRoot(gitDir)
	if err != nil {
		return nil, fmt.Errorf("gitdir: %w", err)
	}

	roots := []string{canonGitDir}
	if common := gitCommonDir(canonGitDir); common != "" {
		canonCommon, err := canonicalizeRoot(common)
		if err != nil {
			return nil, fmt.Errorf("commondir: %w", err)
		}
		roots = append(roots, canonCommon)
	}
	return dedupeGitRoots(roots), nil
}

func gitCommonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		return ""
	}
	if filepath.IsAbs(common) {
		return common
	}
	return filepath.Join(gitDir, common)
}

func dedupeGitRoots(roots []string) []string {
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	return out
}

func agentStateRoot(sub, fallback string) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return fallback
	}
	canon, err := canonicalizeRoot(filepath.Join(home, sub))
	if err != nil {
		return fallback
	}
	return canon
}

// stripEnvKeys returns env with any "KEY=..." entries for the given keys
// removed, preserving the relative order of the remaining entries.
func stripEnvKeys(env []string, keys ...string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// resolveReasoningEffort returns the effort level a run dispatches with.
// Precedence: an effort the caller already pinned > the operator's
// agent.role_effort override for this role > the role's built-in baseline >
// the global default.
//
// The pinned tier covers both an A/B experiment assignment and the task's own
// reasoning_effort, resolved upstream because only the dispatch site knows
// which of the two applies — a dispatch site holding a task.Task must pass
// t.ReasoningEffort through, or the baseline silently outranks the pin.
//
// This lives in the Manager rather than at each dispatch site on purpose:
// before #2784 only two of the nine RunConfig construction sites resolved the
// role baseline, so monitor and human-review ran at "medium" despite
// defaulting to "low", and implementation re-dispatches on the
// limit/failover paths silently dropped their baseline. Resolving here means a
// new dispatch site cannot reintroduce that leak.
func (m *Manager) resolveReasoningEffort(role Role, effort string) string {
	if effort != "" {
		return effort
	}
	m.mu.RLock()
	override, ok := m.roleEffort[string(role)]
	m.mu.RUnlock()
	if ok {
		if valid, err := task.ValidateReasoningEffort(override); err == nil && valid != "" {
			return valid
		}
	}
	if roleDefault := role.DefaultReasoningEffort(); roleDefault != "" {
		return roleDefault
	}
	return DefaultReasoningEffort
}

func validateRunDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("agent.Run: Dir is required (empty Dir would leak agent process into Sybra cwd)")
	}
	if info, err := os.Stat(dir); err != nil {
		return fmt.Errorf("agent.Run: Dir %q not accessible: %w", dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("agent.Run: Dir %q is not a directory", dir)
	}
	return nil
}

func newRunningAgent(id string, cfg RunConfig, prov Provider, cancel context.CancelFunc) *Agent {
	now := time.Now().UTC()
	a := &Agent{
		ID:                      id,
		TaskID:                  cfg.TaskID,
		Name:                    cfg.Name,
		Role:                    cfg.Role,
		Mode:                    cfg.Mode,
		Provider:                prov.Name(),
		Model:                   cfg.resolvedModel,
		RequestedModel:          cfg.Model,
		ReasoningEffort:         cfg.ReasoningEffort,
		RequestedSkill:          cfg.RequestedSkill,
		SkillExecutionMode:      cfg.SkillExecutionMode,
		ResolvedSkillSourceHash: cfg.ResolvedSkillSourceHash,
		SkillConformance:        cfg.SkillConformance,
		OutputSchema:            cfg.OutputSchema,
		skillRecoveryAttempt:    cfg.SkillRecoveryAttempt,
		// Only a provider that actually forwards OutputSchema to its CLI makes
		// the conformance receipt unsatisfiable; copilot/opencode ignore it, so
		// their runs still get (and must pass) receipt verification. Mirror the
		// exact wantReceipt condition in resolveWorkflowSkillPrompt.
		hasOutputSchema: cfg.OutputSchema != "" && prov.EnforcesOutputSchema(),
		Prompt:          cfg.Prompt,
		// Stamp the canonical dispatch prompt hash centrally for every run
		// (all providers, all roles, both modes). cfg.Prompt is the fully
		// prepared prompt (post NOTES.md/guardrail/skill preparation) — the
		// same value recordImplAgentStart hashes. Stamping here (not only in
		// the implementation-only dispatch path) is what lets completion emit
		// agent.prompt_rendered for review/fix-review/pr-fix/human-review/
		// workflow runs, which also record provider render summaries.
		promptHash:             skillattr.HashSourceID(cfg.Prompt),
		State:                  StateRunning,
		StartedAt:              now,
		LastEventAt:            now,
		cancel:                 cancel,
		sessionCWD:             cfg.Dir,
		sandboxHomeDir:         cfg.resolvedSandboxHome,
		MaxTurns:               cfg.MaxTurns,
		oneShot:                cfg.OneShot,
		requirePermissions:     cfg.RequirePermissions,
		sandboxMode:            cfg.SandboxMode,
		headlessPermissionMode: cfg.HeadlessPermissionMode,
	}
	if cfg.ResumeSessionID != "" {
		a.SetSessionID(cfg.ResumeSessionID)
	}
	if cfg.Mode == "headless" {
		a.done = make(chan struct{})
	}
	if cfg.Mode == "headless" {
		a.escalationCh = make(chan bool, 1)
	}
	a.setAssignment(cfg)
	return a
}

func (m *Manager) registerRunningAgent(a *Agent, cfg RunConfig, cancel context.CancelFunc) error {
	class := a.EffectiveRole().WorkloadClass()
	m.mu.Lock()
	reserved := false
	if !cfg.IgnoreConcurrencyLimit && cfg.capacityReservation != nil && cfg.capacityReservation.manager == m {
		reserved = cfg.capacityReservation.consumeLocked()
	}
	if !cfg.IgnoreConcurrencyLimit && !reserved && !admitClass(class, m.liveByClass, m.reservedByClass, m.classFloors, m.maxConcurrent) {
		m.mu.Unlock()
		cancel()
		metrics.AgentClassRejected(string(class))
		return fmt.Errorf("%w (%d)", ErrMaxConcurrentReached, m.maxConcurrent)
	}
	borrowed := !reserved && borrowsSharedCapacity(class, m.liveByClass, m.reservedByClass, m.classFloors)
	m.agents[a.ID] = a
	if a.done != nil {
		m.liveCount++
		if a.Provider != "" {
			m.liveByProvider[a.Provider]++
		}
		m.liveByClass[class]++
	}
	m.mu.Unlock()
	// Only count dispatches that actually passed through the capacity gate
	// (!IgnoreConcurrencyLimit) and were capacity-tracked (a.done != nil, i.e.
	// added to m.liveByClass). Interactive agents and gate-bypassing callers
	// (monitor/loop/orchestrator/human-review set IgnoreConcurrencyLimit) never
	// enter the class gauges, so folding them into "admitted"/"borrowed" would
	// misreport class-isolation behavior for anyone debugging starvation.
	if !cfg.IgnoreConcurrencyLimit && a.done != nil {
		metrics.AgentClassAdmitted(string(class))
		if borrowed {
			metrics.AgentClassBorrowed(string(class))
		}
	}
	return nil
}

func (m *Manager) startAgentRunner(ctx context.Context, a *Agent, cfg RunConfig, prov Provider, cancel context.CancelFunc) error {
	switch cfg.Mode {
	case "headless":
		m.mu.RLock()
		k8sRunner := m.k8sRunner
		m.mu.RUnlock()
		if k8sRunner != nil {
			go k8sRunner.Run(ctx, m, a, cfg)
			return nil
		}
		go m.runHeadless(ctx, a, cfg)
	default:
		cancel()
		return fmt.Errorf("unknown mode: %s", cfg.Mode)
	}
	return nil
}

func (m *Manager) markAgentDone(ctx context.Context, a *Agent) {
	if a == nil || a.done == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
		if a.isDetached() {
			reapProcessGroup(a.GetPID())
		}
		// Close the stdin pipe/FIFO and drop the registry record + FIFO file
		// so a completed agent leaks neither an fd nor an on-disk pipe.
		a.convo.closeStdinPipe()
		if reg := m.registry(); reg != nil {
			_ = reg.Delete(a.ID)
		}
		m.mu.Lock()
		if m.liveCount > 0 {
			m.liveCount--
		}
		if a.Provider != "" {
			if v, ok := m.liveByProvider[a.Provider]; ok {
				if v <= 1 {
					delete(m.liveByProvider, a.Provider)
				} else {
					m.liveByProvider[a.Provider] = v - 1
				}
			}
		}
		if class := a.EffectiveRole().WorkloadClass(); m.liveByClass[class] > 0 {
			m.liveByClass[class]--
		}
		retention := m.deadAgentRetention
		m.mu.Unlock()
		m.signalQueueNudge()
		if ctx == nil {
			ctx = context.Background()
		}
		if roots := orphanSweepRootsForAgent(a); len(roots) > 0 {
			m.ReapOrphanProviderProcesses(ctx, roots)
		}

		// Evict the finished agent from the live registry so its output
		// buffer and prompt do not accumulate forever on a long-lived
		// server. All completion side effects (recordCompletion/fireComplete,
		// task-status advancement, stats persistence) already ran before
		// markAgentDone was called. Eviction is delayed by deadAgentRetention
		// rather than immediate, since callers routinely read final state
		// (GetAgent/GetConvoOutput/Output) in the seconds right after a
		// terminal transition (e.g. StopAgent's caller polling for
		// StateStopped) — evicting synchronously here would race that
		// read and turn a normal completion into a "not found" error.
		evict := func() {
			m.mu.Lock()
			// Only delete the entry we scheduled eviction for, i.e. do not
			// remove an agent whose id was reused by a still-live
			// registration in the meantime.
			if cur, ok := m.agents[a.ID]; ok && cur == a {
				delete(m.agents, a.ID)
			}
			m.mu.Unlock()
		}
		if retention <= 0 {
			evict()
		} else {
			time.AfterFunc(retention, evict)
		}
	})
}

func (m *Manager) buildCommand(cfg RunConfig) (string, error) {
	resolved, err := m.providerForRun(cfg.Provider)
	if err != nil {
		return "", err
	}
	prov, err := lookupProvider(resolved)
	if err != nil {
		return "", err
	}
	model, err := resolvedModelForRun(cfg, prov)
	if err != nil {
		return "", err
	}
	if model != "" && !safeArgRe.MatchString(model) {
		return "", fmt.Errorf("invalid model %q: must match %s", cfg.Model, safeArgRe)
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			return "", fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
		}
	}
	return prov.BuildCommand(cfg, model), nil
}

// ResolveProvider predicts the provider a Run call with this cfg would
// dispatch to, applying the same health-gate / limit-gate failover logic as
// prepareRunConfig. Callers that need to make provider-scoped decisions
// before Run actually runs (e.g. selecting a resumable session) can use this
// to avoid assuming the requested/default provider always wins. Unlike
// gateProvider, this never emits metrics or logs — it only computes the
// decision — so calling it ahead of Run and then again inside Run cannot
// double-emit AgentGated/AgentFailover metrics or agent.run.* log lines,
// though the two calls can in principle disagree if gate state changes in
// between.
func (m *Manager) ResolveProvider(cfg RunConfig) (string, error) {
	resolved, _, _, err := m.resolveProviderDecision(cfg)
	return resolved, err
}

// providerGateEvent records a metrics/log side effect that gateProvider
// would emit for a given routing decision, deferred so resolveProviderDecision
// can stay side-effect free and be reused by both gateProvider and the
// prediction-only ResolveProvider.
type providerGateEvent struct {
	kind      string // "gated" or "failover"
	provider  string // gated: the gated provider
	from, to  string // failover: source and destination provider
	reason    string
	altReason string
	logKey    string // failover: log message key
	logLevel  string // failover: "warn" or "info"
	taskID    string
}

// gateProvider resolves the run's provider through the health gate. If the
// configured provider is unhealthy and auto-failover can supply a healthy
// peer, the peer is returned. Otherwise returns a typed UnhealthyError so
// callers can detect via errors.Is(err, provider.ErrProviderUnhealthy).
func (m *Manager) gateProvider(cfg RunConfig) (string, error) {
	resolved, _, gateEvents, err := m.resolveProviderDecision(cfg)
	m.emitProviderGateEvents(gateEvents)
	return resolved, err
}

// emitProviderGateEvents applies the metrics/log side effects recorded by
// resolveProviderDecision. Only gateProvider calls this — ResolveProvider
// discards the events so prediction stays observation-free.
func (m *Manager) emitProviderGateEvents(gateEvents []providerGateEvent) {
	for i := range gateEvents {
		e := &gateEvents[i]
		switch e.kind {
		case "gated":
			metrics.AgentGated(e.provider, e.reason)
		case "soft_limit":
			m.logger.Info(e.logKey, "provider", e.provider, "reason", e.reason, "task", e.taskID)
		case "failover":
			metrics.AgentFailover(e.from, e.to)
			fields := []any{"from", e.from, "to", e.to, "task", e.taskID}
			if e.reason != "" {
				fields = append(fields, "reason", e.reason)
			}
			if e.altReason != "" {
				fields = append(fields, "alt_reason", e.altReason)
			}
			if e.logLevel == "warn" {
				m.logger.Warn(e.logKey, fields...)
			} else {
				m.logger.Info(e.logKey, fields...)
			}
		}
	}
}

// resolveProviderDecision computes the provider a Run call with this cfg
// would dispatch to, applying health-gate / limit-gate failover logic. It is
// side-effect free: metrics/log emissions are returned as providerGateEvent
// values for the caller to apply (gateProvider) or discard (ResolveProvider).
func (m *Manager) resolveProviderDecision(cfg RunConfig) (resolvedProvider, routingReason string, gateEvents []providerGateEvent, err error) {
	resolved, err := m.providerForRun(cfg.Provider)
	if err != nil {
		return "", "", nil, err
	}
	routingReason = cfg.RoutingReason
	if routingReason == "" {
		if strings.TrimSpace(cfg.Provider) == "" {
			routingReason = "default"
		} else {
			routingReason = "explicit"
		}
	}
	if cfg.IgnoreHealthGate {
		return resolved, routingReason, nil, nil
	}
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	maxInFlight := m.maxInFlightPerProvider
	live := maps.Clone(m.liveByProvider)
	m.mu.RUnlock()
	enabled := func(p string) bool {
		return providerPolicyEnabled(lp, p)
	}
	underCap := func(p string) bool {
		return maxInFlight <= 0 || live[p] < maxInFlight
	}
	healthy := func(p string) bool {
		return enabled(p) && (g == nil || g.IsHealthy(p)) && underCap(p)
	}
	candidateProviders := providerid.All()
	if !enabled(resolved) {
		var disabledEvents []providerGateEvent
		resolved, routingReason, disabledEvents, err = failoverDisabledProvider(resolved, routingReason, cfg, candidateProviders, healthy)
		gateEvents = append(gateEvents, disabledEvents...)
		if err != nil {
			return "", routingReason, gateEvents, err
		}
	}
	if g != nil && !g.IsHealthy(resolved) {
		var healthEvents []providerGateEvent
		resolved, routingReason, healthEvents, err = failoverUnhealthyProvider(resolved, routingReason, cfg, candidateProviders, g, healthy)
		gateEvents = append(gateEvents, healthEvents...)
		if err != nil {
			return "", routingReason, gateEvents, err
		}
	}
	if lg == nil {
		return resolved, routingReason, gateEvents, nil
	}
	if ok, reason := lg.ProviderAvailable(resolved, lp); ok {
		if !cfg.DisableProviderFailover {
			// The soft in-flight cap must redirect regardless of
			// PreferUnderused and regardless of quota data: the limits
			// store's ChooseProvider only recommends an alternative from
			// exact quota-pressure data (see limits.Store.ChooseProvider),
			// so it silently no-ops when the requested provider merely sits
			// at its in-flight cap with no quota signal. Picking the first
			// healthy, under-cap candidate directly — bypassing that
			// quota heuristic — is what makes the cap actually redirect.
			if !underCap(resolved) {
				if alt := firstHealthyProvider(resolved, candidateProviders, healthy); alt != "" {
					gateEvents = append(gateEvents, providerGateEvent{
						kind: "failover", from: resolved, to: alt, logKey: "agent.run.cap_redirect", logLevel: "info", taskID: cfg.TaskID,
					})
					return alt, "limit", gateEvents, nil
				}
			} else if lp.PreferUnderused {
				if alt, altReason := lg.ChooseProvider(resolved, candidateProviders, healthy, lp); alt != "" {
					gateEvents = append(gateEvents, providerGateEvent{
						kind: "failover", from: resolved, to: alt, reason: altReason, logKey: "agent.run.limit_select", logLevel: "info", taskID: cfg.TaskID,
					})
					return alt, "limit", gateEvents, nil
				}
			}
		}
		return resolved, routingReason, gateEvents, nil
	} else if !cfg.DisableProviderFailover {
		if alt, altReason := lg.ChooseProvider(resolved, candidateProviders, healthy, lp); alt != "" {
			gateEvents = append(gateEvents, providerGateEvent{
				kind: "failover", from: resolved, to: alt, reason: reason, altReason: altReason, logKey: "agent.run.limit_failover", logLevel: "warn", taskID: cfg.TaskID,
			})
			return alt, "limit", gateEvents, nil
		}
		// No fully available peer exists. Before failing closed, check
		// whether a peer is only soft-threshold limited (e.g. near its
		// session cap but still dispatching, the same leniency
		// softLimitLastResort grants the resolved provider itself) — that
		// peer is only a safe last-resort failover target when resolved is
		// hard-blocked (e.g. rate limit actually reached). If resolved is
		// itself only soft-threshold limited, keep it so the remaining
		// budget is not stranded behind another soft-limited peer.
		if !limits.IsSoftThresholdReason(reason) {
			if alt, altReason := lg.ChooseSoftLimitedPeer(resolved, candidateProviders, healthy, lp); alt != "" {
				gateEvents = append(gateEvents, providerGateEvent{
					kind: "failover", from: resolved, to: alt, reason: reason, altReason: altReason, logKey: "agent.run.soft_limit_peer_failover", logLevel: "warn", taskID: cfg.TaskID,
				})
				return alt, "limit", gateEvents, nil
			}
		}
		return m.softLimitLastResort(resolved, routingReason, reason, gateEvents, cfg.TaskID)
	} else {
		return m.softLimitLastResort(resolved, routingReason, reason, gateEvents, cfg.TaskID)
	}
}

func providerPolicyEnabled(policy limits.Policy, providerName string) bool {
	if ok, exists := policy.ProviderEnabled[providerName]; exists && !ok {
		return false
	}
	return true
}

func failoverDisabledProvider(resolved, routingReason string, cfg RunConfig, candidates []string, healthy func(string) bool) (selectedProvider, selectedRoutingReason string, gateEvents []providerGateEvent, err error) {
	reason := "provider disabled"
	if cfg.DisableProviderFailover {
		return "", routingReason,
			[]providerGateEvent{{kind: "gated", provider: resolved, reason: reason}},
			newProviderUnhealthy(resolved, reason)
	}
	alt := firstHealthyProvider(resolved, candidates, healthy)
	if alt == "" {
		return "", routingReason,
			[]providerGateEvent{{kind: "gated", provider: resolved, reason: reason}},
			newProviderUnhealthy(resolved, reason)
	}
	return alt, "failover",
		[]providerGateEvent{{
			kind: "failover", from: resolved, to: alt,
			reason: reason, logKey: "agent.run.failover", logLevel: "warn", taskID: cfg.TaskID,
		}},
		nil
}

func failoverUnhealthyProvider(resolved, routingReason string, cfg RunConfig, candidates []string, g provider.HealthGate, healthy func(string) bool) (selectedProvider, selectedRoutingReason string, gateEvents []providerGateEvent, err error) {
	reason := g.Reason(resolved)
	if cfg.DisableProviderFailover {
		return "", routingReason,
			[]providerGateEvent{{kind: "gated", provider: resolved, reason: reason}},
			newProviderUnhealthy(resolved, reason)
	}
	alt := g.Failover(resolved)
	if alt != "" && !healthy(alt) {
		// g.Failover only consults the health checker. Runtime policy and
		// in-flight caps live in the agent manager, so revalidate the pick
		// before accepting it as a runnable provider.
		alt = firstHealthyProvider(resolved, candidates, healthy)
	}
	if alt == "" {
		return "", routingReason,
			[]providerGateEvent{{kind: "gated", provider: resolved, reason: reason}},
			newProviderUnhealthy(resolved, reason)
	}
	altProv, err := lookupProvider(alt)
	if err != nil {
		return "", routingReason, nil, err
	}
	return altProv.Name(), "failover",
		[]providerGateEvent{{
			kind: "failover", from: resolved, to: altProv.Name(),
			reason: reason, logKey: "agent.run.failover", logLevel: "warn", taskID: cfg.TaskID,
		}},
		nil
}

func (m *Manager) softLimitLastResort(resolved, routingReason, reason string, gateEvents []providerGateEvent, taskID string) (selectedProvider, selectedRoutingReason string, updatedGateEvents []providerGateEvent, err error) {
	if limits.IsSoftThresholdReason(reason) {
		gateEvents = append(gateEvents, providerGateEvent{
			kind: "soft_limit", provider: resolved, reason: reason, logKey: "agent.run.soft_limit_last_resort", taskID: taskID,
		})
		return resolved, routingReason, gateEvents, nil
	}
	gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
	return "", routingReason, gateEvents, newProviderUnhealthy(resolved, reason)
}

func newProviderUnhealthy(prov, reason string) *provider.UnhealthyError {
	return &provider.UnhealthyError{
		Provider:    prov,
		Reason:      reason,
		RateLimited: reason == provider.RateLimitReason || limits.IsRateLimitReachedReason(reason),
	}
}

// firstHealthyProvider returns a uniformly random candidate (other than
// exclude) for which healthy reports true, or "" if none qualify. Used for
// the in-flight cap redirect, which picks without consulting the limits
// store's quota-pressure heuristics — but must still not always land on the
// same peer, so no single provider is systematically favored or starved.
func firstHealthyProvider(exclude string, candidates []string, healthy func(string) bool) string {
	eligible := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if p == exclude {
			continue
		}
		if healthy(p) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		return ""
	}
	return eligible[rand.IntN(len(eligible))]
}

func (m *Manager) providerForRun(name string) (string, error) {
	m.mu.RLock()
	def := m.defaultProv
	m.mu.RUnlock()
	if name == "" {
		name = def
	}
	prov, err := lookupProvider(name)
	if err != nil {
		return "", err
	}
	return prov.Name(), nil
}

// safeArgRe matches only characters safe to embed in a shell command
// without quoting: alphanumerics, dot, underscore, hyphen, forward-slash.
var safeArgRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// claudeDurableConfigPaths lists the parts of claude's state dir that decide
// how *future* runs behave: settings.json carries PreToolUse hooks, and
// hooks/ holds their scripts. A run that edits either changes every later
// run, including independent verifier roles, and survives worktree cleanup —
// the one thing a per-task sandbox is supposed to prevent.
//
// Everything else in the dir stays writable because the CLI genuinely uses
// it: a single multi-turn run writes plugins/, projects/, sessions/,
// session-env/ and shell-snapshots/. Narrowing to an allowlist instead broke
// runs outright (#2779).
//
// Absent paths are skipped: binding a path that does not exist fails the
// spawn, and a missing settings.json is nothing to protect.
func claudeDurableConfigPaths(stateRoot string) []string {
	if strings.TrimSpace(stateRoot) == "" {
		return nil
	}
	var out []string
	for _, name := range []string{"settings.json", "settings.local.json", "hooks"} {
		p := filepath.Join(stateRoot, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
