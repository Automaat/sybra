package agent

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/google/uuid"
)

func (m *Manager) StartAgent(taskID, taskTitle, mode, prompt, dir string, allowedTools []string) (*Agent, error) {
	return m.Run(RunConfig{TaskID: taskID, Name: taskTitle, Mode: mode, Prompt: prompt, AllowedTools: allowedTools, Dir: dir})
}

func (a *Agent) setAssignment(cfg RunConfig) {
	a.ExperimentID = cfg.ExperimentID
	a.VariantID = cfg.VariantID
	a.AssignmentUnit = cfg.AssignmentUnit
	a.AssignmentKey = cfg.AssignmentKey
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
	if m.survives() && willDetach(cfg) {
		a.setDetached(true)
	}

	if err := m.registerRunningAgent(a, cfg, cancel); err != nil {
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

func (m *Manager) prepareRunConfig(cfg RunConfig) (RunConfig, Provider, error) {
	if err := validateRunDir(cfg.Dir); err != nil {
		return cfg, nil, err
	}
	if cfg.SeedWorkingMemory {
		cfg.Prompt = notes.SeedPrompt(cfg.Prompt, cfg.Dir)
	}
	cfg.Prompt = withBackgroundTaskGuardrail(cfg.Prompt, cfg)
	resolvedProvider, gateErr := m.gateProvider(cfg)
	if gateErr != nil {
		return cfg, nil, gateErr
	}
	prov, providerErr := lookupProvider(resolvedProvider)
	if providerErr != nil {
		return cfg, nil, providerErr
	}
	cfg.provider = prov
	cfg.ReasoningEffort = defaultReasoningEffort(cfg.ReasoningEffort)
	if cfg.Mode == "headless" {
		m.mu.RLock()
		steerable := m.headlessSteerable
		m.mu.RUnlock()
		// Verifier/system roles and fix-review dispatch unattended (no caller
		// ever writes a steer message) — see Role.SupportsHeadlessSteer.
		cfg.HeadlessSteerable = steerable && RoleFromName(cfg.Name).SupportsHeadlessSteer()
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
// see #1576. System/probe runs with an empty TaskID (health checks,
// orchestrator-internal probes) are the only ones allowed to skip this: they
// have no task-scoped worktree/sandbox to isolate into.
//
// cfg.ExtraEnv is normalized before the trusted values are appended: any
// existing SYBRA_HOME/SYBRA_CONTROL_HOME entries (caller-supplied or
// otherwise) are stripped first, then the sandbox home and (if configured) the
// control home are appended last, so they always win regardless of duplicate
// env var resolution order in the target process.
func (m *Manager) injectSandboxHome(cfg *RunConfig) error {
	if cfg.TaskID == "" {
		return nil
	}
	m.mu.RLock()
	resolve := m.sandboxHome
	controlHome := m.controlHome
	m.mu.RUnlock()
	if resolve == nil {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "err", "no sandbox home resolver configured")
		return fmt.Errorf("agent.Run: no sandbox home resolver configured for task-scoped run %q", cfg.TaskID)
	}
	dir, err := resolve(cfg.TaskID)
	if err != nil {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: resolve sandbox home for task %q: %w", cfg.TaskID, err)
	}
	if strings.TrimSpace(dir) == "" {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "err", "resolver returned empty path")
		return fmt.Errorf("agent.Run: sandbox home resolver returned empty path for task %q", cfg.TaskID)
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = fmt.Errorf("%q is not a directory", dir)
		}
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "err", statErr)
		return fmt.Errorf("agent.Run: sandbox home %q for task %q is not accessible: %w", dir, cfg.TaskID, statErr)
	}

	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "SYBRA_HOME", "SYBRA_CONTROL_HOME")
	cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_HOME="+dir)
	if controlHome != "" {
		cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_CONTROL_HOME="+controlHome)
	}
	cfg.resolvedSandboxHome = dir
	return nil
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
	cfg.ExtraEnv = append(cfg.ExtraEnv, "GH_TOKEN="+token, "GITHUB_TOKEN="+token)
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
	goBuild := buildcache.TaskGoBuildDir(cfg.TaskID)
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
// and allowed write roots (worktree, per-task sandbox home, tmp) into
// cfg.sandbox, applied later by wrapInvocation at each provider spawn site.
//
// enforce fails closed — mirroring injectSandboxHome's discipline — when
// sandbox-exec is unavailable, the embedded profile cannot be materialized,
// or a root cannot be canonicalized: the error aborts the run before any
// subprocess spawns. report never blocks: the same failures are logged and
// this run's spec falls back to "off" (unwrapped) instead of erroring, so a
// misconfigured or unsupported host cannot break the default posture.
func (m *Manager) injectProcessSandbox(cfg *RunConfig) error {
	mode, err := config.NormalizeSandboxMode(cfg.SandboxMode)
	if err != nil {
		return fmt.Errorf("agent.Run: sandbox mode: %w", err)
	}
	cfg.SandboxMode = mode
	if mode == "off" {
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}

	worktree := cfg.Dir
	sandboxHome := cfg.resolvedSandboxHome
	if sandboxHome == "" {
		sandboxHome = worktree
	}
	tmp := os.TempDir()
	sharedCache := sharedBuildCacheDir()

	if !sandboxExecAvailable() {
		if mode == "enforce" {
			err := fmt.Errorf("agent.Run: enforce sandbox mode requires sandbox-exec, which is unavailable on this host")
			m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
			return err
		}
		m.logger.Warn("agent.sandbox.report.unavailable", "task_id", cfg.TaskID)
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}

	if mode != "enforce" {
		// report: log the resolved allowlist without wrapping the spawn, so
		// the opaque provider CLI's would-be write footprint is visible from
		// live runs before flipping this task/fleet to enforce. Canonicalize
		// the roots the same way enforce mode would, so the report output is
		// representative of what enforce would allow — but never fail the
		// run closed on a canonicalization error; fall back to logging the
		// raw (unresolved) roots instead.
		logWorktree, logSandboxHome, logTmp, logShared := worktree, sandboxHome, tmp, sharedCache
		if canon, err := canonicalizeRoot(worktree); err == nil {
			logWorktree = canon
		} else {
			m.logger.Warn("agent.sandbox.report.canonicalize_failed", "task_id", cfg.TaskID, "root", "worktree", "err", err)
		}
		if canon, err := canonicalizeRoot(sandboxHome); err == nil {
			logSandboxHome = canon
		} else {
			m.logger.Warn("agent.sandbox.report.canonicalize_failed", "task_id", cfg.TaskID, "root", "sandbox_home", "err", err)
		}
		if canon, err := canonicalizeRoot(tmp); err == nil {
			logTmp = canon
		} else {
			m.logger.Warn("agent.sandbox.report.canonicalize_failed", "task_id", cfg.TaskID, "root", "tmp", "err", err)
		}
		if canon, err := canonicalizeRoot(sharedCache); err == nil {
			logShared = canon
		} else {
			m.logger.Warn("agent.sandbox.report.canonicalize_failed", "task_id", cfg.TaskID, "root", "shared_cache", "err", err)
		}
		m.logger.Info("agent.sandbox.report", "task_id", cfg.TaskID,
			"worktree", logWorktree, "sandbox_home", logSandboxHome, "tmp", logTmp, "shared_cache", logShared)
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}

	canonWorktree, err := canonicalizeRoot(worktree)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox worktree root: %w", err)
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
	if err := os.MkdirAll(sharedCache, 0o755); err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: create sandbox shared-cache root: %w", err)
	}
	canonSharedCache, err := canonicalizeRoot(sharedCache)
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox shared-cache root: %w", err)
	}
	profilePath, err := materializeSandboxProfile()
	if err != nil {
		m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox profile: %w", err)
	}

	cfg.sandbox = enforceSpec(canonWorktree, canonSandboxHome, canonTmp, canonSharedCache, profilePath)
	m.logger.Info("agent.sandbox.enforce", "task_id", cfg.TaskID,
		"worktree", canonWorktree, "sandbox_home", canonSandboxHome, "tmp", canonTmp,
		"shared_cache", canonSharedCache, "claude_state", cfg.sandbox.claudeState,
		"codex_state", cfg.sandbox.codexState, "copilot_state", cfg.sandbox.copilotState,
		"tool_cache", cfg.sandbox.toolCache, "profile", profilePath)
	return nil
}

func enforceSpec(worktree, sandboxHome, tmp, sharedCache, profilePath string) sandboxSpec {
	return sandboxSpec{
		mode:         "enforce",
		worktree:     worktree,
		sandboxHome:  sandboxHome,
		tmp:          tmp,
		sharedCache:  sharedCache,
		profilePath:  profilePath,
		claudeState:  agentStateRoot(".claude", sandboxHome),
		codexState:   agentStateRoot(".codex", sandboxHome),
		copilotState: agentStateRoot(".copilot", sandboxHome),
		toolCache:    agentStateRoot(".cache", sandboxHome),
	}
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

func defaultReasoningEffort(effort string) string {
	if effort != "" {
		return effort
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
		ID:                     id,
		TaskID:                 cfg.TaskID,
		Name:                   cfg.Name,
		Mode:                   cfg.Mode,
		Provider:               prov.Name(),
		Model:                  prov.NormalizeModel(cfg.Model),
		ReasoningEffort:        cfg.ReasoningEffort,
		Prompt:                 cfg.Prompt,
		State:                  StateRunning,
		StartedAt:              now,
		LastEventAt:            now,
		cancel:                 cancel,
		sessionCWD:             cfg.Dir,
		MaxTurns:               cfg.MaxTurns,
		oneShot:                cfg.OneShot,
		requirePermissions:     cfg.RequirePermissions,
		sandboxMode:            cfg.SandboxMode,
		headlessPermissionMode: cfg.HeadlessPermissionMode,
	}
	if cfg.ResumeSessionID != "" {
		a.SetSessionID(cfg.ResumeSessionID)
	}
	if cfg.Mode == "headless" || cfg.Mode == "interactive" {
		a.done = make(chan struct{})
	}
	if cfg.Mode == "headless" {
		a.escalationCh = make(chan bool, 1)
	}
	a.setAssignment(cfg)
	return a
}

func (m *Manager) registerRunningAgent(a *Agent, cfg RunConfig, cancel context.CancelFunc) error {
	m.mu.Lock()
	if !cfg.IgnoreConcurrencyLimit && m.maxConcurrent > 0 && m.liveCount >= m.maxConcurrent {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("%w (%d)", ErrMaxConcurrentReached, m.maxConcurrent)
	}
	m.agents[a.ID] = a
	if a.done != nil {
		m.liveCount++
		if a.Provider != "" {
			m.liveByProvider[a.Provider]++
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) startAgentRunner(ctx context.Context, a *Agent, cfg RunConfig, prov Provider, cancel context.CancelFunc) error {
	switch cfg.Mode {
	case "headless":
		go m.runHeadless(ctx, a, cfg)
	case "interactive":
		// codex and copilot use the per-turn conversational runner (each turn
		// spawns a fresh process); claude uses the persistent approval-hook
		// runner. Copilot's permission model is CLI-flag based (no HTTP
		// approval hook), so the per-turn shape fits it like codex.
		if prov.UsesPerTurnConvo() {
			a.setPromptChannel(make(chan string, 1))
			go m.runPerTurnConversational(ctx, a, cfg, false)
		} else {
			go m.runConversational(ctx, a, cfg)
		}
	default:
		cancel()
		return fmt.Errorf("unknown mode: %s", cfg.Mode)
	}
	return nil
}

func (m *Manager) markAgentDone(a *Agent) {
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
		retention := m.deadAgentRetention
		m.mu.Unlock()
		m.signalQueueNudge()

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
	model := prov.NormalizeModel(cfg.Model)
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
	resolved, _, err := m.resolveProviderDecision(cfg)
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
	resolved, gateEvents, err := m.resolveProviderDecision(cfg)
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
func (m *Manager) resolveProviderDecision(cfg RunConfig) (string, []providerGateEvent, error) {
	resolved, err := m.providerForRun(cfg.Provider)
	if err != nil {
		return "", nil, err
	}
	if cfg.IgnoreHealthGate {
		return resolved, nil, nil
	}
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	maxInFlight := m.maxInFlightPerProvider
	live := maps.Clone(m.liveByProvider)
	m.mu.RUnlock()
	underCap := func(p string) bool {
		return maxInFlight <= 0 || live[p] < maxInFlight
	}
	healthy := func(p string) bool {
		return (g == nil || g.IsHealthy(p)) && underCap(p)
	}
	var gateEvents []providerGateEvent
	candidateProviders := providerid.All()
	if g != nil && !g.IsHealthy(resolved) {
		if cfg.DisableProviderFailover {
			reason := g.Reason(resolved)
			gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
			return "", gateEvents, newProviderUnhealthy(resolved, reason)
		}
		alt := g.Failover(resolved)
		if alt != "" && !underCap(alt) {
			// g.Failover only consults health, so its pick may already be at
			// its in-flight cap. AutoFailover being enabled is established by
			// alt != "", so it's safe to look for another peer that is both
			// healthy and under cap before settling for the at-cap pick.
			if capAlt := firstHealthyProvider(resolved, candidateProviders, healthy); capAlt != "" {
				alt = capAlt
			}
		}
		if alt != "" {
			altProv, err := lookupProvider(alt)
			if err != nil {
				return "", gateEvents, err
			}
			gateEvents = append(gateEvents, providerGateEvent{
				kind: "failover", from: resolved, to: altProv.Name(),
				reason: g.Reason(resolved), logKey: "agent.run.failover", logLevel: "warn", taskID: cfg.TaskID,
			})
			resolved = altProv.Name()
		} else {
			reason := g.Reason(resolved)
			gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
			return "", gateEvents, newProviderUnhealthy(resolved, reason)
		}
	}
	if lg == nil {
		return resolved, gateEvents, nil
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
					return alt, gateEvents, nil
				}
			} else if lp.PreferUnderused {
				if alt, altReason := lg.ChooseProvider(resolved, candidateProviders, healthy, lp); alt != "" {
					gateEvents = append(gateEvents, providerGateEvent{
						kind: "failover", from: resolved, to: alt, reason: altReason, logKey: "agent.run.limit_select", logLevel: "info", taskID: cfg.TaskID,
					})
					return alt, gateEvents, nil
				}
			}
		}
		return resolved, gateEvents, nil
	} else if !cfg.DisableProviderFailover {
		if alt, altReason := lg.ChooseProvider(resolved, candidateProviders, healthy, lp); alt != "" {
			gateEvents = append(gateEvents, providerGateEvent{
				kind: "failover", from: resolved, to: alt, reason: reason, altReason: altReason, logKey: "agent.run.limit_failover", logLevel: "warn", taskID: cfg.TaskID,
			})
			return alt, gateEvents, nil
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
				return alt, gateEvents, nil
			}
		}
		return m.softLimitLastResort(resolved, reason, gateEvents, cfg.TaskID)
	} else {
		return m.softLimitLastResort(resolved, reason, gateEvents, cfg.TaskID)
	}
}

func (m *Manager) softLimitLastResort(resolved, reason string, gateEvents []providerGateEvent, taskID string) (string, []providerGateEvent, error) {
	if limits.IsSoftThresholdReason(reason) {
		gateEvents = append(gateEvents, providerGateEvent{
			kind: "soft_limit", provider: resolved, reason: reason, logKey: "agent.run.soft_limit_last_resort", taskID: taskID,
		})
		return resolved, gateEvents, nil
	}
	gateEvents = append(gateEvents, providerGateEvent{kind: "gated", provider: resolved, reason: reason})
	return "", gateEvents, newProviderUnhealthy(resolved, reason)
}

func newProviderUnhealthy(prov, reason string) *provider.UnhealthyError {
	return &provider.UnhealthyError{
		Provider:    prov,
		Reason:      reason,
		RateLimited: reason == provider.RateLimitReason || limits.IsRateLimitReachedReason(reason),
	}
}

// firstHealthyProvider returns the first candidate (other than exclude) for
// which healthy reports true, or "" if none qualify. Used for the in-flight
// cap redirect, which must pick deterministically without consulting the
// limits store's quota-pressure heuristics.
func firstHealthyProvider(exclude string, candidates []string, healthy func(string) bool) string {
	for _, p := range candidates {
		if p == exclude {
			continue
		}
		if healthy(p) {
			return p
		}
	}
	return ""
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
