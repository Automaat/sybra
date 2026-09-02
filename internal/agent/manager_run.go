package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/toolledger"
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
	a.unthrottledOutputEvents = cfg.UnthrottledOutputEvents
}

func (m *Manager) Run(cfg RunConfig) (*Agent, error) {
	return m.RunContext(m.ctx, cfg)
}

func (m *Manager) RunContext(ctx context.Context, cfg RunConfig) (*Agent, error) {
	if err := m.jitterRunDispatchContext(ctx, cfg); err != nil {
		return nil, err
	}

	cfg, prov, err := m.prepareRunConfig(cfg) //nolint:contextcheck // provider gating emits via manager-owned app lifecycle, not per-run ctx
	if err != nil {
		return nil, err
	}
	if err := m.certifyPreparedRun(ctx, cfg, prov.Name()); err != nil {
		return nil, err
	}

	id := uuid.NewString()[:8]
	intent := attemptIntentForRun(cfg, prov.Name())
	lease, err := m.acquireAttempt(ctx, intent)
	if err != nil {
		return nil, err
	}
	if lease.Existing {
		return nil, fmt.Errorf("%w: intent %s already admitted as lease %s", ErrAttemptConflict, intent.IntentID, lease.ID)
	}
	ctx, cancel := context.WithCancel(ctx)
	a := newRunningAgent(id, cfg, prov, cancel)
	a.attemptIntent = intent
	a.attemptTaskGenKnown = intent.TaskID != ""
	a.attemptLease = lease
	a.SetToolCallRecorder(m.recordToolCall)
	if err := m.bindAttempt(ctx, a, ""); err != nil {
		cancel()
		m.completeAttempt(ctx, a, "bind_failed")
		return nil, fmt.Errorf("agent.Run: bind attempt: %w", err)
	}
	if cfg.BeforeStart != nil {
		if err := cfg.BeforeStart(id); err != nil {
			cancel()
			m.completeAttempt(ctx, a, "before_start_failed")
			return nil, fmt.Errorf("agent.Run: before start: %w", err)
		}
	}
	cfg = injectProcessOwnerEnv(cfg, processOwnerForAgent(a))
	if m.survives() && willDetach(cfg) {
		a.setDetached(true)
	}

	if err := m.registerRunningAgent(a, cfg, cancel); err != nil { //nolint:contextcheck // per-class metrics are process-global accounting, not tied to per-run ctx
		m.completeAttempt(ctx, a, "registration_failed")
		return nil, err
	}

	metrics.AgentStarted(a.Provider, a.Mode) //nolint:contextcheck // metrics are process-global accounting, not tied to per-run ctx
	m.logger.Info("agent.start", "id", id, "taskID", cfg.TaskID, "mode", cfg.Mode, "provider", a.Provider, "model", a.Model)

	if err := m.startAgentRunner(ctx, a, cfg, prov, cancel); err != nil {
		a.SetExitErr(err)
		a.SetState(StateStopped)
		m.completeAttempt(ctx, a, "start_failed")
		m.markAgentDone(ctx, a)
		m.emit(events.AgentState(id), a)
		return nil, err
	}
	m.startAttemptHeartbeat(ctx, a)

	m.emit(events.AgentState(id), a)
	return a, nil
}

func attemptIntentForRun(cfg RunConfig, providerName string) AttemptIntent {
	access := cfg.AttemptAccess
	if access == "" {
		access = AttemptAccessMutate
	}
	if cfg.ReadOnlyDir {
		access = AttemptAccessObserve
	}
	intentID := strings.TrimSpace(cfg.IntentID)
	if intentID == "" {
		// Only an explicit workflow/effect identity is replay-stable. A
		// deterministic fallback would make recurring taskless monitor and loop
		// runs replay a lease that already completed. The generated identity is
		// persisted in the registry, so restart adoption remains stable.
		intentID = "dispatch:" + uuid.NewString()
	}
	return AttemptIntent{
		IntentID: intentID, TaskID: firstNonEmpty(cfg.AdmissionTaskKey, cfg.TaskID), TaskGeneration: cfg.TaskGeneration,
		Worktree: firstNonEmpty(cfg.AdmissionWorktree, cfg.Dir), WorktreeGeneration: cfg.WorktreeGeneration,
		Access: access, Role: cfg.Role, Provider: providerName,
		CapabilityCertified: true,
	}
}

func (m *Manager) admissionService() AttemptAdmission {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.attemptAdmission
}

// NeedsAttemptReconciliation reports whether durable admission contains an
// expired lease. Recovery uses this to avoid an expensive process/worktree
// preservation pass on ordinary maintenance ticks.
func (m *Manager) NeedsAttemptReconciliation(ctx context.Context) bool {
	reconciler, ok := m.admissionService().(AttemptLedgerReconciler)
	if !ok {
		return false
	}
	needed, err := reconciler.NeedsReconciliation(ctx)
	if err != nil {
		m.logger.Warn("agent.attempt.reconcile-check", "err", err)
		return false
	}
	return needed
}

// ReconcileAttemptLeases finalizes expired leases not represented by either
// the survival registry or the live manager. Callers must first reap owned
// unregistered processes and preserve worktrees.
func (m *Manager) ReconcileAttemptLeases(ctx context.Context) int {
	reconciler, ok := m.admissionService().(AttemptLedgerReconciler)
	if !ok {
		return 0
	}
	seen := make(map[string]AttemptLease)
	if reg := m.registry(); reg != nil {
		records, err := reg.List()
		if err != nil {
			m.logger.Warn("agent.attempt.reconcile-registry", "err", err)
			return 0
		}
		for i := range records {
			if records[i].AttemptLeaseID != "" {
				seen[records[i].AttemptLeaseID] = AttemptLease{ID: records[i].AttemptLeaseID, Version: records[i].AttemptVersion}
			}
		}
	}
	m.mu.RLock()
	for _, a := range m.agents {
		a.mu.RLock()
		lease := a.attemptLease
		a.mu.RUnlock()
		if lease.ID != "" && a.GetState() != StateStopped {
			seen[lease.ID] = lease
		}
	}
	m.mu.RUnlock()
	observed := make([]AttemptLease, 0, len(seen))
	for _, lease := range seen {
		observed = append(observed, lease)
	}
	n, err := reconciler.ReconcileUnobserved(ctx, observed)
	if err != nil {
		m.logger.Warn("agent.attempt.reconcile", "err", err)
		return 0
	}
	if n > 0 {
		m.logger.Info("agent.attempt.reconciled", "count", n)
		if m.controlEvent != nil {
			m.controlEvent("attempt_leases.reconciled", map[string]any{"count": n, "outcome": "orphan_reconciled"})
		}
	}
	return n
}

func (m *Manager) acquireAttempt(ctx context.Context, intent AttemptIntent) (AttemptLease, error) {
	admission := m.admissionService()
	if admission == nil {
		return AttemptLease{}, nil
	}
	return admission.Acquire(ctx, intent)
}

func (m *Manager) bindAttempt(ctx context.Context, a *Agent, procStarted string) error {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	lease := a.attemptLease
	binding := AttemptBinding{
		AgentID: a.ID, PID: a.PID, ProcStarted: procStarted,
		SessionID: a.SessionID, ObservedAt: time.Now().UTC(),
	}
	a.mu.RUnlock()
	admission := m.admissionService()
	if admission == nil || lease.ID == "" {
		return nil
	}
	return admission.Bind(ctx, lease, binding)
}

func (m *Manager) bindAndHeartbeatAttempt(ctx context.Context, a *Agent, procStarted string) {
	if err := m.bindAttempt(ctx, a, procStarted); err != nil {
		m.logger.Warn("agent.attempt.bind", "id", a.ID, "err", err)
		return
	}
	a.mu.RLock()
	lease := a.attemptLease
	a.mu.RUnlock()
	admission := m.admissionService()
	if admission == nil || lease.ID == "" {
		return
	}
	if err := admission.Heartbeat(ctx, lease, time.Now().UTC()); err != nil {
		m.logger.Warn("agent.attempt.heartbeat", "id", a.ID, "err", err)
	}
}

const attemptHeartbeatInterval = 15 * time.Second

func (m *Manager) startAttemptHeartbeat(ctx context.Context, a *Agent) {
	if m.admissionService() == nil || a == nil {
		return
	}
	a.mu.RLock()
	hasLease := a.attemptLease.ID != ""
	a.mu.RUnlock()
	if !hasLease {
		return
	}
	go func() {
		ticker := time.NewTicker(attemptHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.mu.RLock()
				lease := a.attemptLease
				a.mu.RUnlock()
				if admission := m.admissionService(); admission != nil {
					if err := admission.Heartbeat(ctx, lease, time.Now().UTC()); err != nil {
						m.logger.Warn("agent.attempt.heartbeat", "id", a.ID, "err", err)
					}
				}
			case <-a.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (m *Manager) completeAttempt(ctx context.Context, a *Agent, outcome string) {
	if a == nil {
		return
	}
	a.attemptCompleteOnce.Do(func() {
		a.mu.RLock()
		lease := a.attemptLease
		a.mu.RUnlock()
		admission := m.admissionService()
		if admission == nil || lease.ID == "" {
			return
		}
		if ctx == nil {
			ctx = context.Background()
		} else {
			ctx = context.WithoutCancel(ctx)
		}
		if err := admission.Complete(ctx, lease, outcome); err != nil {
			m.logger.Warn("agent.attempt.complete", "id", a.ID, "outcome", outcome, "err", err)
		}
	})
}

func attemptTerminalOutcome(a *Agent) string {
	if a.GetExitErr() != nil {
		return "failed"
	}
	if a.WasStopped() {
		return string(taskstatus.Cancelled)
	}
	return "completed"
}

func attemptIntentFromRecord(r Record) AttemptIntent {
	access := r.AttemptAccess
	if access == "" {
		access = AttemptAccessMutate
	}
	intentID := r.AttemptIntentID
	if intentID == "" {
		intentID = "legacy-registry:" + r.ID
	}
	return AttemptIntent{
		IntentID: intentID, TaskID: firstNonEmpty(r.AttemptTaskKey, r.TaskID),
		TaskGeneration: r.AttemptTaskGen, Worktree: firstNonEmpty(r.AttemptWorktree, r.CWD),
		WorktreeGeneration: r.AttemptWorkGen, Access: access,
		Role: r.Role, Provider: r.Provider, CapabilityCertified: true,
	}
}

func (m *Manager) adoptAttempt(ctx context.Context, r Record, intent AttemptIntent) (AttemptLease, error) {
	admission := m.admissionService()
	lease := AttemptLease{ID: r.AttemptLeaseID, Version: r.AttemptVersion}
	if admission == nil {
		return lease, nil
	}
	binding := AttemptBinding{
		AgentID: r.ID, PID: r.PID, ProcStarted: r.ProcStartedAt,
		SessionID: r.SessionID, ObservedAt: time.Now().UTC(),
	}
	if lease.ID == "" {
		acquired, err := admission.Acquire(ctx, intent)
		if err != nil {
			return AttemptLease{}, err
		}
		if acquired.Existing {
			return AttemptLease{}, fmt.Errorf("%w: legacy registry intent %s", ErrAttemptConflict, intent.IntentID)
		}
		if err := admission.Bind(ctx, acquired, binding); err != nil {
			_ = admission.Complete(context.WithoutCancel(ctx), acquired, "legacy_bind_failed")
			return AttemptLease{}, err
		}
		return acquired, nil
	}
	return admission.Adopt(ctx, intent, lease, binding)
}

func (m *Manager) certifyPreparedRun(ctx context.Context, cfg RunConfig, providerName string) error {
	m.mu.RLock()
	preflight := m.runEnvironmentPreflight
	m.mu.RUnlock()
	if preflight == nil {
		return nil
	}
	return preflight(ctx, RunEnvironment{
		TaskID: cfg.TaskID, Role: cfg.Role, Dir: cfg.Dir, ReadOnlyPaths: slices.Clone(cfg.ReadOnlyPaths), Provider: providerName,
		GitRoots:     slices.Clone(cfg.GitRoots),
		SandboxMode:  cfg.SandboxMode,
		ScratchRoots: preparedScratchRoots(cfg),
	})
}

func preparedScratchRoots(cfg RunConfig) []string {
	roots := []string{
		cfg.resolvedSandboxHome,
		os.TempDir(),
		sharedBuildCacheDir(),
		buildcache.TaskGoBuildDir(cfg.TaskID),
		buildcache.SharedGoModDir(),
		buildcache.SharedNPMDir(),
	}
	if cfg.resolvedSandboxHome != "" {
		roots = append(roots, filepath.Join(cfg.resolvedSandboxHome, "golangci-lint-cache"))
	}
	return compactPaths(roots)
}

func compactPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
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

func (m *Manager) jitterRunDispatchContext(ctx context.Context, cfg RunConfig) error {
	if cfg.Mode != "headless" || cfg.SkipDispatchJitter {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.jitterDispatchContext(ctx)
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
	if err := m.resolveAttemptGeneration(&cfg); err != nil {
		return cfg, nil, err
	}
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
	m.reconcileResumeSessionProvider(&cfg, requestedProvider, prov.Name())
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
	if prov.Name() == providerid.Claude && cfg.Mode == "headless" &&
		cfg.RequirePermissions && cfg.approvalAddr == "" &&
		len(cfg.AllowedTools) == 0 && cfg.HeadlessPermissionMode != "auto" {
		return cfg, nil, fmt.Errorf("require_permissions requires a running approval server for ungated headless claude runs")
	}

	if err := m.injectSandboxHome(&cfg); err != nil {
		return cfg, nil, err
	}
	if err := m.injectShellTempPrefix(&cfg); err != nil {
		return cfg, nil, err
	}
	if err := injectScratchEnvironment(&cfg); err != nil {
		return cfg, nil, err
	}

	if err := m.injectGitAccess(&cfg); err != nil {
		return cfg, nil, err
	}

	if err := m.injectGolangciCache(&cfg); err != nil {
		return cfg, nil, err
	}

	if err := m.injectSharedBuildCache(&cfg); err != nil {
		return cfg, nil, err
	}

	// Independent verification is only trustworthy when project-controlled
	// code is actually contained. Verifier roles therefore fail closed under
	// enforce regardless of the rollout posture used for author agents.
	if err := m.enforceAndContain(&cfg); err != nil {
		return cfg, nil, err
	}

	m.preparePlaywrightMCP(&cfg)

	m.applyRuntimeDefaults(&cfg)
	return cfg, prov, nil
}

// reconcileResumeSessionProvider drops a provider-local session when the final
// dispatch gate selects a different provider than prediction did. Health,
// quota, or capacity can change during dispatch jitter; carrying the stale ID
// across that failover would make the selected CLI reject it before receiving
// the prompt.
func (m *Manager) reconcileResumeSessionProvider(cfg *RunConfig, requestedProvider, selectedProvider string) {
	if cfg.ResumeSessionID == "" {
		return
	}
	resumeProvider := cfg.ResumeSessionProvider
	if resumeProvider == "" {
		// Backward-compatible safety for callers that predate the explicit
		// session-owner field: their requested provider was also the provider
		// used to choose the session.
		resumeProvider = requestedProvider
	}
	if resumeProvider == selectedProvider {
		return
	}
	m.logger.Info("agent.run.resume_session_dropped",
		"task_id", cfg.TaskID,
		"from", resumeProvider,
		"to", selectedProvider,
	)
	cfg.ResumeSessionID = ""
	cfg.ResumeSessionProvider = ""
}

func (m *Manager) applyRuntimeDefaults(cfg *RunConfig) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cfg.BashTimeoutMs == 0 {
		cfg.BashTimeoutMs = m.bashTimeoutMs
	}
	if cfg.RetryWatchdog == 0 {
		cfg.RetryWatchdog = m.retryWatchdog
	}
	if cfg.FallbackModel == "" {
		cfg.FallbackModel = m.fallbackModel
	}
}

func (m *Manager) resolveAttemptGeneration(cfg *RunConfig) error {
	if cfg == nil || cfg.TaskID == "" {
		return nil
	}
	m.mu.RLock()
	generation := m.taskGeneration
	m.mu.RUnlock()
	if generation == nil {
		return nil
	}
	value, ok := generation(cfg.TaskID)
	if !ok || value <= 0 {
		return nil
	}
	current := uint64(value)
	if cfg.TaskGeneration != 0 && cfg.TaskGeneration != current {
		return fmt.Errorf("%w: task %s generation %d is stale (current %d)", ErrAttemptConflict, cfg.TaskID, cfg.TaskGeneration, current)
	}
	cfg.TaskGeneration = current
	if cfg.WorktreeGeneration == 0 {
		cfg.WorktreeGeneration = current
	}
	return nil
}

func (m *Manager) injectGitAccess(cfg *RunConfig) error {
	if cfg.Role.IsVerifier() {
		if cfg.Role == RoleReview && m.allowsAmbientReviewAuth() && !m.hasVerifierGitHubToken() {
			// This is a deliberately explicit escape hatch for an operator's
			// local gh login. Keep the approval guard: it still blocks APPROVE,
			// while the disposable verifier workspace has its push remote disabled.
			m.injectAmbientReviewGhShim(cfg)
			return nil
		}
		if err := isolateVerifierGitCredentials(cfg); err != nil {
			return err
		}
		if cfg.Role == RoleReview {
			m.injectVerifierGhShim(cfg)
			if err := m.injectVerifierGitHubToken(cfg); err != nil {
				return err
			}
			m.grantVerifierGhReadPaths(cfg)
		}
		return nil
	}
	m.injectGhShim(cfg)
	m.injectGitHubToken(cfg)
	return nil
}

func (m *Manager) allowsAmbientReviewAuth() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.allowAmbientReviewAuth
}

func (m *Manager) hasVerifierGitHubToken() bool {
	m.mu.RLock()
	tokenFn := m.ghVerifierAppToken
	m.mu.RUnlock()
	return tokenFn != nil && tokenFn() != ""
}

func (m *Manager) injectVerifierGitHubToken(cfg *RunConfig) error {
	m.mu.RLock()
	tokenFn := m.ghVerifierAppToken
	m.mu.RUnlock()
	if tokenFn == nil {
		if cfg.TaskID != "" {
			return errors.New("agent.Run: PR review requires a restricted GitHub App verifier token")
		}
		return nil
	}
	token := tokenFn()
	if token == "" {
		if cfg.TaskID != "" {
			return errors.New("agent.Run: restricted GitHub App verifier token is unavailable")
		}
		return nil
	}
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "GH_TOKEN", "GITHUB_TOKEN")
	cfg.ExtraEnv = append(cfg.ExtraEnv, "GH_TOKEN="+token, "GITHUB_TOKEN="+token)
	return nil
}

func (m *Manager) grantVerifierGhReadPaths(cfg *RunConfig) {
	if m.ghShimDir != "" {
		cfg.ReadOnlyPaths = append(cfg.ReadOnlyPaths, filepath.Join(m.ghShimDir, "verifier"))
	}
	for _, name := range []string{"gh"} {
		if path, err := exec.LookPath(name); err == nil {
			cfg.ReadOnlyPaths = append(cfg.ReadOnlyPaths, path)
		}
	}
}

func enforceVerifierSandbox(cfg RunConfig) RunConfig {
	if cfg.Role.IsVerifier() {
		cfg.SandboxMode = "enforce"
		cfg.SandboxReadMode = "enforce"
	}
	return cfg
}

// injectSandboxHome routes every task-scoped agent subprocess's default
// SYBRA_HOME through the per-task sandbox home, so no fresh agent (any
// provider, any role) can resolve the operator's real ~/.sybra by default —
// see #1576. Taskless system runs skip this only when IsolateHome is false;
// long-lived/judgment agents such as the orchestrator opt in so stale Sybra
// source checkouts or sybra-cli invocations cannot rewrite the operator config.
//
// cfg.ExtraEnv is normalized before the trusted values are appended: any
// existing control variables (caller-supplied or otherwise) are stripped
// first. Ordinary roles receive the configured control home; verifiers receive
// only the authenticated API target and token, so the operator store is never
// one of their filesystem roots.
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
	controlTarget := m.controlTarget
	controlToken := m.controlToken
	m.mu.RUnlock()
	if resolve == nil && strings.TrimSpace(cfg.EphemeralSandboxHome) == "" {
		m.logger.Error("agent.sandbox_home.failed", "task_id", cfg.TaskID, "sandbox_key", sandboxKey, "err", "no sandbox home resolver configured")
		return fmt.Errorf("agent.Run: no sandbox home resolver configured for run %q", sandboxKey)
	}
	dir := strings.TrimSpace(cfg.EphemeralSandboxHome)
	var err error
	if dir == "" {
		dir, err = resolve(sandboxKey)
	}
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

	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "SYBRA_HOME", "SYBRA_CONTROL_HOME", "SYBRA_CONTROL_API_ONLY", "SYBRA_SERVER_TARGET", "SYBRA_AUTH_TOKEN", "SYBRA_AUTH_TOKEN_FILE")
	cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_HOME="+dir)
	if cfg.Role.IsVerifier() {
		if !cfg.DisableVerifierControl && controlTarget != "" && controlToken != nil {
			token := controlToken(cfg.TaskID, dir)
			if token == "" {
				return errors.New("agent.Run: verifier control token is unavailable")
			}
			tokenPath := VerifierControlTokenPath(dir)
			if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
				return fmt.Errorf("agent.Run: write verifier control token: %w", err)
			}
			cfg.ExtraEnv = append(cfg.ExtraEnv,
				"SYBRA_CONTROL_API_ONLY=1",
				"SYBRA_SERVER_TARGET="+controlTarget,
				"SYBRA_AUTH_TOKEN_FILE="+tokenPath,
			)
		}
	} else if target, token, ca := m.board(); target != "" && token != "" {
		// Named, not inferred. The CLI has no filesystem path to a board any
		// more, and everything it would infer one from — the recorded port
		// file, the config beside it — lives in the operator home, which is
		// off the read allowlist for every role but monitor. Under an
		// enforcing read sandbox the CLI cannot even load that config, so it
		// dies before it looks at a target.
		//
		// SYBRA_CONTROL_HOME is therefore deliberately not exported here: its
		// only job was to point the CLI at the operator home, and a named
		// board makes that pointer worse than useless. The token and the CA
		// go in the sandbox home, readable by construction, exactly as the
		// verifier channel does it.
		tokenPath := filepath.Join(dir, boardTokenFile)
		if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
			return fmt.Errorf("agent.Run: write board token: %w", err)
		}
		cfg.ExtraEnv = append(cfg.ExtraEnv,
			"SYBRA_SERVER_TARGET="+target,
			"SYBRA_AUTH_TOKEN_FILE="+tokenPath,
		)
		if ca != "" {
			// A board serving TLS signs its own certificate, so the system
			// roots reject it and the agent has no way to read the operator's
			// copy. Copied in rather than pointed at for the same reason.
			caPath := filepath.Join(dir, boardCAFile)
			pem, readErr := os.ReadFile(ca)
			if readErr != nil {
				return fmt.Errorf("agent.Run: read board certificate: %w", readErr)
			}
			if err := os.WriteFile(caPath, pem, 0o600); err != nil {
				return fmt.Errorf("agent.Run: write board certificate: %w", err)
			}
			cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_SERVER_CA="+caPath)
		}
	} else {
		// No board named. The CLI will refuse every call the agent makes, so
		// say which run it was rather than leaving a paid run to fail with
		// nothing in the log tying it to this.
		m.logger.Warn("agent.board.unnamed", "task_id", cfg.TaskID, "role", string(cfg.Role))
		if controlHome != "" {
			cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_CONTROL_HOME="+controlHome)
		}
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

const scratchHomePrompt = `## Temporary files

When a command or test needs a disposable HOME, use $SYBRA_SCRATCH_HOME. For
ordinary scratch files - command output you read back, intermediate JSON, a
query you build up - use $SYBRA_SCRATCH_DIR. Both live outside the Git worktree.

Do not write to /tmp. It is world-writable and shared with unrelated tasks, so
the sandbox does not grant it and the write fails with a permission error.
Never create fake homes, caches, or other runtime state inside the worktree.`

// shellTempPrefixEnv is zsh's temp-file prefix, and the reason a contained run
// can use a heredoc at all. zsh writes every heredoc body to a file under
// $TMPPREFIX, and macOS's system zsh compiles that to /tmp/zsh and never
// re-derives it from TMPDIR. Both sandboxes grant the per-user temp root but
// not /tmp, so under enforce every heredoc died with "can't create temp file
// for here document: operation not permitted" — taking away the ordinary way
// an agent writes multi-line content and pushing it onto improvised fallbacks
// (#3377). Pointing the prefix at an already-granted directory restores it.
const shellTempPrefixEnv = "TMPPREFIX"

// injectShellTempPrefix points shellTempPrefixEnv at a writable directory in
// the run's sandbox home. The prefix names a file stem, not a directory, so
// zsh appends its own suffix to produce <sandbox-home>/zsh/zshXXXXXX and only
// the parent has to exist.
//
// Deliberately a peer of scratch-home rather than a directory inside it: the
// run's own prompt advertises $SYBRA_SCRATCH_HOME as disposable, so an agent
// that cleans up after itself would delete the prefix directory and lose
// heredocs for the rest of the run.
//
// Fails soft. Losing the prefix costs heredocs under enforce, which is the
// state every run was already in; refusing to dispatch over it would turn one
// stray file in an agent-writable directory into a task that can never run
// again, on every posture including off.
func (m *Manager) injectShellTempPrefix(cfg *RunConfig) error {
	if strings.TrimSpace(cfg.resolvedSandboxHome) == "" {
		return nil
	}
	// Strip first. A caller-supplied prefix can name any writable path,
	// including one inside the worktree, so a directory this cannot create
	// must leave the run with no prefix at all rather than that one.
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, shellTempPrefixEnv)
	dir := filepath.Join(cfg.resolvedSandboxHome, "zsh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if m.resolvedSandboxModeFor(cfg) == "enforce" {
			return fmt.Errorf("agent.Run: create shell temp prefix directory %q: %w", dir, err)
		}
		m.logger.Warn("agent.shell_temp_prefix.failed", "task_id", cfg.TaskID, "dir", dir, "err", err)
		return nil
	}
	cfg.ExtraEnv = append(cfg.ExtraEnv, shellTempPrefixEnv+"="+filepath.Join(dir, "zsh"))
	return nil
}

// injectScratchEnvironment gives every sandbox-home-backed run an explicit
// place for fake user homes outside the Git worktree. HOME itself deliberately
// remains unchanged: provider CLIs use it to find their authenticated
// ~/.claude, ~/.codex, or ~/.copilot state.
func injectScratchEnvironment(cfg *RunConfig) error {
	if strings.TrimSpace(cfg.resolvedSandboxHome) == "" {
		// A taskless, non-isolated system run keeps the caller's environment
		// untouched (TestPrepareRunConfig_SandboxHome_SystemRunSkipsInjection),
		// and runs no agent shell. Every run that does gets a sandbox home.
		return nil
	}
	scratchHome := filepath.Join(cfg.resolvedSandboxHome, "scratch-home")
	if err := os.MkdirAll(scratchHome, 0o700); err != nil {
		return fmt.Errorf("agent.Run: create task scratch directory %q: %w", scratchHome, err)
	}
	scratchDir := filepath.Join(cfg.resolvedSandboxHome, "scratch")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return fmt.Errorf("agent.Run: create task scratch file directory %q: %w", scratchDir, err)
	}
	// Do not replace TMPDIR/TMP/TEMP here. Provider and harness control files
	// may already live beneath the caller's process temp root; changing that
	// root makes those paths fail validation and can change CLI behaviour.
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "SYBRA_SCRATCH_HOME", "SYBRA_SCRATCH_DIR")
	cfg.ExtraEnv = append(cfg.ExtraEnv, "SYBRA_SCRATCH_HOME="+scratchHome, "SYBRA_SCRATCH_DIR="+scratchDir)
	if !strings.Contains(cfg.Prompt, scratchHomePrompt) {
		if cfg.Prompt != "" {
			cfg.Prompt = strings.TrimRight(cfg.Prompt, "\n") + "\n\n"
		}
		cfg.Prompt += scratchHomePrompt
	}
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
	// Do not snapshot the short-lived App token into the provider process env:
	// agents can run longer than GitHub installation tokens live, and a stale
	// GH_TOKEN wins over the credential helper even after Sybra refreshes its
	// cache. Override any ambient token to empty here; the PATH-level gh shim
	// mints a fresh token for each short-lived gh child process instead.
	cfg.ExtraEnv = append(cfg.ExtraEnv, "GH_TOKEN=", "GITHUB_TOKEN=")
}

// isolateVerifierGitCredentials keeps judge roles capable of exercising Git
// inside their disposable clone without giving them any credential path that
// could publish those private mutations. The process sandbox supplies the
// stronger filesystem boundary; these overrides also prevent Git and gh from
// consulting ambient operator configuration that would otherwise be readable
// through a credential helper.
func isolateVerifierGitCredentials(cfg *RunConfig) error {
	ambientHome := ambientEnvValue(cfg.ExtraEnv, "HOME", "")
	ambientConfig := ambientEnvValue(cfg.ExtraEnv, "XDG_CONFIG_HOME", filepath.Join(ambientHome, ".config"))
	ambientMiseData := ambientEnvValue(cfg.ExtraEnv, "MISE_DATA_DIR", filepath.Join(ambientHome, ".local", "share", "mise"))
	isolationRoot := strings.TrimSpace(cfg.resolvedSandboxHome)
	if isolationRoot == "" {
		// Taskless verifier probes do not receive a separate sandbox home. Their
		// run directory is already the enforce sandbox's writable root.
		isolationRoot = cfg.Dir
	}
	isolatedConfig := filepath.Join(isolationRoot, ".config")
	if err := os.MkdirAll(filepath.Join(isolatedConfig, "git"), 0o700); err != nil {
		return fmt.Errorf("agent.Run: create verifier config home: %w", err)
	}
	cfg.ExtraEnv = stripEnvKeyPrefixes(cfg.ExtraEnv, "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_")
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv,
		"HOME", "XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
		"MISE_NO_CONFIG", "MISE_NO_ENV", "MISE_DATA_DIR", "MISE_TRUSTED_CONFIG_PATHS",
		"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_ASKPASS", "SSH_ASKPASS",
	)
	trustedMise := []string{cfg.Dir}
	if ambientConfig != "" {
		trustedMise = append(trustedMise, filepath.Join(ambientConfig, "mise", "config.toml"))
	}
	trustedMise = slices.DeleteFunc(trustedMise, func(path string) bool { return strings.TrimSpace(path) == "" })
	cfg.ExtraEnv = append(cfg.ExtraEnv,
		"HOME="+isolationRoot,
		"XDG_CONFIG_HOME="+isolatedConfig,
		// On macOS mise discovers its global config from the account database,
		// not HOME/XDG_CONFIG_HOME. Trust only that exact file plus the disposable
		// checkout, suppress config-provided environment values, and reuse the read-only
		// installed tool store. MISE_NO_CONFIG is insufficient: mise then removes
		// project-only tools such as golangci-lint from its exec environment.
		"MISE_NO_ENV=1",
		"MISE_TRUSTED_CONFIG_PATHS="+strings.Join(trustedMise, string(os.PathListSeparator)),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=0",
		"GH_TOKEN=", "GITHUB_TOKEN=", "SSH_AUTH_SOCK=", "SSH_AGENT_PID=", "GIT_ASKPASS=", "SSH_ASKPASS=",
	)
	if ambientMiseData != "" {
		cfg.ExtraEnv = append(cfg.ExtraEnv, "MISE_DATA_DIR="+ambientMiseData)
	}
	return nil
}

// dirExists reports whether path is a directory this process can stat.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info == nil {
		return false
	}
	return info.IsDir()
}

// injectMiseDataDir gives a sandboxed run its own mise data directory whose
// installs tree links the operator's, so mise finds every pinned tool already
// present and writes its shim rebuild inside the sandbox.
//
// `mise install` is the first line of many projects' setup, and it rebuilds
// global shims on every run — pruning ones other projects left behind. The
// operator's store is read-only to an agent, so that prune fails with
// "Operation not permitted" and takes the whole verify suite down with it,
// even though every tool was already installed. Redirecting the data dir
// keeps the mutation inside the sandbox rather than widening the write
// allowlist to a directory that can land on an operator's PATH.
// enforceAndContain settles the run's posture, then prepares everything that
// depends on it. The mise mirror comes after enforceVerifierSandbox, never
// before: a verifier run the config left at report is escalated here, and it
// needs the mirror that the enforced posture makes necessary.
func (m *Manager) enforceAndContain(cfg *RunConfig) error {
	*cfg = enforceVerifierSandbox(*cfg)
	if err := m.injectMiseDataDir(cfg); err != nil {
		return err
	}
	return m.injectProcessSandbox(cfg)
}

func (m *Manager) injectMiseDataDir(cfg *RunConfig) error {
	host := hostMiseDataDir()
	// Stripped whatever the outcome: a caller-supplied value otherwise reaches
	// the provider process, and a follower takes this environment from a run
	// spec. A value that was there is replaced by the host store rather than
	// dropped, so a run that expects one still resolves an absolute path.
	replaced := strings.TrimSpace(ambientEnvValue(cfg.ExtraEnv, "MISE_DATA_DIR", "")) != ""
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "MISE_DATA_DIR")
	if cfg.resolvedSandboxHome == "" || cfg.SandboxMode != "enforce" || host == "" {
		if replaced && host != "" {
			cfg.ExtraEnv = append(cfg.ExtraEnv, "MISE_DATA_DIR="+host)
		}
		return nil
	}
	installs := filepath.Join(host, "installs")
	if !dirExists(installs) {
		m.logger.Warn("agent.mise.store-missing", "task_id", cfg.TaskID, "store", host)
		if replaced {
			cfg.ExtraEnv = append(cfg.ExtraEnv, "MISE_DATA_DIR="+host)
		}
		return nil
	}
	dir := filepath.Join(cfg.resolvedSandboxHome, "mise")
	if err := mirrorMiseStore(dir, host); err != nil {
		return fmt.Errorf("agent.Run: mirror mise store for task %q: %w", cfg.TaskID, err)
	}
	cfg.ExtraEnv = append(cfg.ExtraEnv, "MISE_DATA_DIR="+dir)
	return nil
}

// hostMiseDataDir resolves the operator's mise store from this process's own
// environment. It is never read from RunConfig.ExtraEnv: a caller-supplied
// value would point the sandbox's toolchain at a tree of that caller's
// choosing, and every binary mise exec resolves comes out of it.
func hostMiseDataDir() string {
	if configured := strings.TrimSpace(os.Getenv("MISE_DATA_DIR")); filepath.IsAbs(configured) {
		return configured
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "mise")
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); filepath.IsAbs(home) {
		return filepath.Join(home, ".local", "share", "mise")
	}
	return ""
}

// mirrorMiseStore builds a writable mise data directory whose installed tool
// versions and plugins link to the operator's read-only store. Each version
// is linked individually rather than the trees wholesale, so mise can still
// install a version the operator lacks — a toolchain bump would otherwise
// fail writing into the store it cannot touch.
func mirrorMiseStore(dir, host string) error {
	if err := makeMirrorDir(dir); err != nil {
		return err
	}
	for _, tree := range []string{"installs", "plugins"} {
		if err := mirrorMiseTree(filepath.Join(dir, tree), filepath.Join(host, tree), tree == "installs"); err != nil {
			return err
		}
	}
	return nil
}

func mirrorMiseTree(dst, src string, perVersion bool) error {
	if err := makeMirrorDir(dst); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !perVersion {
			if err := linkMiseEntry(filepath.Join(dst, entry.Name()), filepath.Join(src, entry.Name())); err != nil {
				return err
			}
			continue
		}
		if err := mirrorMiseVersions(filepath.Join(dst, entry.Name()), filepath.Join(src, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// mirrorMiseVersions mirrors one tool's version directories. The version
// directory is real and its contents are linked, never the version directory
// itself: mise resolves a tool's bin path by walking that directory, and a
// link yields nothing, so a tool whose binary sits in a nested archive
// directory (golangci-lint, uv, buf, helm) becomes unexecutable.
func mirrorMiseVersions(dst, src string) error {
	if err := makeMirrorDir(dst); err != nil {
		return err
	}
	versions, err := os.ReadDir(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, version := range versions {
		if !version.IsDir() {
			continue
		}
		versionDst := filepath.Join(dst, version.Name())
		if err := makeMirrorDir(versionDst); err != nil {
			return err
		}
		children, err := os.ReadDir(filepath.Join(src, version.Name()))
		if err != nil {
			continue
		}
		for _, child := range children {
			if err := linkMiseEntry(filepath.Join(versionDst, child.Name()), filepath.Join(src, version.Name(), child.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// makeMirrorDir creates one mirror directory, refusing to follow a link an
// earlier run of this task left behind. The sandbox home is writable by the
// agent and outlives the run, while this code runs unsandboxed as the
// operator: following a planted link would delete and relink whatever it
// names (the #1576 class, from the privileged side).
func makeMirrorDir(dir string) error {
	switch info, err := os.Lstat(dir); {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("mise mirror path %s is a symlink", dir)
	case err == nil && !info.IsDir():
		if rmErr := os.Remove(dir); rmErr != nil {
			return rmErr
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// linkMiseEntry points link at target, replacing whatever occupies it. A
// per-task sandbox home outlives the run, so a link left by an earlier run
// can name a store the operator has since moved, and an agent can write over
// the path itself.
func linkMiseEntry(link, target string) error {
	if existing, err := os.Readlink(link); err == nil && existing == target {
		return nil
	}
	if info, err := os.Lstat(link); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		if err := os.RemoveAll(link); err != nil {
			return err
		}
	} else if err == nil {
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return os.Symlink(target, link)
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

// CertificationScratchRoots resolves and materializes the mutable roots that
// prepareRunConfig will expose to a task-scoped provider process. Keeping this
// projection on Manager prevents the run-environment gate from guessing at
// private sandbox-home and build-cache policy.
func (m *Manager) CertificationScratchRoots(taskID string) ([]string, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("certification scratch roots require a task id")
	}
	m.mu.RLock()
	resolve := m.sandboxHome
	m.mu.RUnlock()
	if resolve == nil {
		return nil, errors.New("certification scratch roots require a sandbox home resolver")
	}
	sandboxHome, err := resolve(taskID)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox home: %w", err)
	}
	if strings.TrimSpace(sandboxHome) == "" {
		return nil, errors.New("sandbox home resolver returned an empty path")
	}
	sharedCache := sharedBuildCacheDir()
	roots := compactPaths([]string{
		sandboxHome,
		os.TempDir(),
		sharedCache,
		buildcache.TaskGoBuildDir(taskID),
		buildcache.SharedGoModDir(),
		buildcache.SharedNPMDir(),
		filepath.Join(sandboxHome, "golangci-lint-cache"),
	})
	for _, root := range roots {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("create certification scratch root %q: %w", root, err)
		}
	}
	return roots, nil
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
// resolvedSandboxModeFor reports the posture this run will use, applying the
// manager default when the run names none. Unparseable values read as the
// empty string; injectProcessSandbox reports that error properly.
func (m *Manager) resolvedSandboxModeFor(cfg *RunConfig) string {
	requested := cfg.SandboxMode
	if strings.TrimSpace(requested) == "" {
		m.mu.RLock()
		requested = m.defaultSandboxMode
		m.mu.RUnlock()
	}
	mode, err := config.NormalizeSandboxMode(requested)
	if err != nil {
		return ""
	}
	return mode
}

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

	if mechanismErr := hostSandboxMechanismErr(); mechanismErr != nil {
		if mode == "enforce" {
			err := fmt.Errorf("agent.Run: enforce sandbox mode requires %s, which is unavailable on this host: %w", sandboxWrapperName(), mechanismErr)
			m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
			return err
		}
		m.logger.Warn("agent.sandbox.report.unavailable", "task_id", cfg.TaskID, "err", mechanismErr)
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
	canonSidecarDir := ""
	if strings.TrimSpace(cfg.SidecarDir) != "" {
		canonSidecarDir, err = canonicalizeRoot(cfg.SidecarDir)
		if err != nil {
			m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
			return fmt.Errorf("agent.Run: sandbox sidecar root: %w", err)
		}
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
	cfg.sandbox = enforceSpec(canonWorktree, gitMetadata, canonSandboxHome, canonTmp, sandboxTmpAlias(canonTmp), canonSharedCache, profilePath, "", gitRoots, gitOverlay)
	cfg.sandbox.sidecarDir = canonSidecarDir
	if cfg.DisableVerifierControl {
		clearProviderStateRoots(&cfg.sandbox)
	}
	if err := m.applySandboxReadMode(cfg); err != nil {
		return err
	}
	m.logger.Info("agent.sandbox.enforce", "task_id", cfg.TaskID,
		"worktree", canonWorktree, "sandbox_home", canonSandboxHome, "sidecar_dir", canonSidecarDir, "tmp", canonTmp,
		"tmp_alias", cfg.sandbox.tmpAlias,
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

func clearProviderStateRoots(spec *sandboxSpec) {
	spec.claudeState = ""
	spec.codexState = ""
	spec.copilotState = ""
	spec.opencodeState = ""
	spec.toolCache = ""
	spec.appSupport = ""
	spec.claudeScratch = ""
	spec.stateDenied = nil
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

	if mechanismErr := hostSandboxMechanismErr(); mechanismErr != nil {
		if mode == "enforce" {
			err := fmt.Errorf("agent.Run: enforce sandbox mode requires %s, which is unavailable on this host: %w", sandboxWrapperName(), mechanismErr)
			m.logger.Error("agent.sandbox.failed", "task_id", cfg.TaskID, "err", err)
			return err
		}
		m.logger.Warn("agent.sandbox.report.unavailable", "task_id", cfg.TaskID, "err", mechanismErr)
		cfg.sandbox = sandboxSpec{mode: "off"}
		return nil
	}
	if mode != "enforce" {
		logTmp := m.reportSandboxRoot(cfg.TaskID, "tmp", tmp)
		m.logger.Info("agent.sandbox.report.readonly_dir", "task_id", cfg.TaskID, "dir", dir,
			"sandbox_home", m.reportSandboxRoot(cfg.TaskID, "sandbox_home", sandboxHome),
			"tmp", logTmp,
			"tmp_alias", sandboxTmpAlias(logTmp),
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
	cfg.sandbox = enforceSpec("", nil, canonSandboxHome, canonTmp, sandboxTmpAlias(canonTmp), canonSharedCache, profilePath, canonDir, gitSandboxRoots{}, gitSandboxOverlay{})
	m.logger.Info("agent.sandbox.enforce.readonly_dir", "task_id", cfg.TaskID,
		"dir", canonDir, "sandbox_home", canonSandboxHome, "tmp", canonTmp,
		"tmp_alias", cfg.sandbox.tmpAlias,
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
		"worktree", logWorktree, "sandbox_home", logSandboxHome, "tmp", logTmp, "tmp_alias", sandboxTmpAlias(logTmp), "shared_cache", logShared,
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
	sandboxHome, tmp, tmpAlias, sharedCache, profilePath, readOnlyDir string,
	gitRoots gitSandboxRoots,
	gitOverlay gitSandboxOverlay,
) sandboxSpec {
	branchRefFile, branchRefLockFile, branchLogFile, _ := gitBranchSingleFiles(gitRoots)
	commonFiles := gitCommonDirSingleFiles(gitRoots)
	return sandboxSpec{
		mode:                        "enforce",
		worktree:                    worktree,
		gitMetadata:                 slices.Clone(gitMetadata),
		gitShared:                   slices.Clone(gitRoots.sharedWritable),
		gitReadonly:                 slices.Clone(gitRoots.sharedReadonly),
		sandboxHome:                 sandboxHome,
		tmp:                         tmp,
		tmpAlias:                    tmpAlias,
		sharedCache:                 sharedCache,
		profilePath:                 profilePath,
		readOnlyDir:                 readOnlyDir,
		gitAdminDir:                 gitRoots.adminDir,
		gitCommonDir:                gitRoots.commonDir,
		gitWorktrees:                gitRoots.worktreesDir,
		gitObjectDir:                gitRoots.objectDir,
		gitLooseObjectPattern:       gitLooseObjectPattern(gitRoots.objectDir),
		gitLooseObjectFanoutPattern: gitLooseObjectFanoutPattern(gitRoots.objectDir),
		gitBranchRef:                gitRoots.branchRef,
		gitBranchRefDir:             gitRoots.branchRefDir,
		gitBranchLogDir:             gitRoots.branchLogDir,
		gitBranchRefFile:            branchRefFile,
		gitBranchRefLockFile:        branchRefLockFile,
		gitBranchLogFile:            branchLogFile,
		gitPackedRefsLockFile:       commonFiles.packedRefsLock,
		gitShallowFile:              commonFiles.shallow,
		gitShallowLockFile:          commonFiles.shallowLock,
		gitStashRefFile:             commonFiles.stashRef,
		gitStashRefLockFile:         commonFiles.stashRefLock,
		gitStashLogFile:             commonFiles.stashLog,
		gitStashLogLockFile:         commonFiles.stashLogLock,
		gitRemoteRefDir:             gitRoots.remoteRefDir,
		gitRemoteLogDir:             gitRoots.remoteLogDir,
		gitRemoteLogLockPattern:     gitLogLockPattern(gitRoots.remoteLogDir),
		gitTagRefDir:                gitRoots.tagRefDir,
		gitTagLogDir:                gitRoots.tagLogDir,
		gitTagLogLockPattern:        gitLogLockPattern(gitRoots.tagLogDir),
		gitNotesRefDir:              gitRoots.notesRefDir,
		gitNotesLogDir:              gitRoots.notesLogDir,
		gitNotesLogLockPattern:      gitLogLockPattern(gitRoots.notesLogDir),
		gitOverlayObjectDir:         gitOverlay.objectDir,
		gitOverlayRefDir:            gitOverlay.branchRefDir,
		gitOverlayLogDir:            gitOverlay.branchLogDir,
		gitOverlayRefFile:           gitOverlay.branchRefFile,
		gitOverlayRemoteRefDir:      gitOverlay.remoteRefDir,
		gitOverlayRemoteLogDir:      gitOverlay.remoteLogDir,
		gitOverlayTagRefDir:         gitOverlay.tagRefDir,
		gitOverlayTagLogDir:         gitOverlay.tagLogDir,
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
		appSupport:    providerAppSupportRoot(),
		claudeScratch: claudeScratchRoot(),
	}
}

func gitLooseObjectPattern(objectDir string) string {
	if objectDir == "" {
		return ""
	}
	canonicalName := "(" + strings.Repeat("[0-9a-f]", 38) + "|" + strings.Repeat("[0-9a-f]", 62) + ")"
	return "^" + regexp.QuoteMeta(objectDir) + `/(tmp_obj_[^/]+|[0-9a-f][0-9a-f]/(tmp_obj_[^/]+|` + canonicalName + `))$`
}

func gitLogLockPattern(logDir string) string {
	if logDir == "" {
		return ""
	}
	return "^" + regexp.QuoteMeta(logDir) + `/.*\.lock$`
}

func gitLooseObjectFanoutPattern(objectDir string) string {
	if objectDir == "" {
		return ""
	}
	canonicalName := "(" + strings.Repeat("[0-9a-f]", 38) + "|" + strings.Repeat("[0-9a-f]", 62) + ")"
	return "^" + regexp.QuoteMeta(objectDir) + `/[0-9a-f][0-9a-f]/` + canonicalName + `$`
}

// gitBranchSingleFiles derives the exact, single-file absolute paths for the
// current branch's ref, its lock, and its reflog (plus the reflog's own lock
// — `git reflog expire`, part of `git gc`, locks it same as the ref) from
// roots' already-resolved directories — narrow enough to grant directly on
// darwin (see sandboxSpec's gitBranchRefFile doc). Empty on a detached HEAD,
// matching roots.branchRef.
func gitBranchSingleFiles(roots gitSandboxRoots) (refFile, refLockFile, logFile, logLockFile string) {
	if roots.branchRef == "" || roots.branchRefDir == "" || roots.branchLogDir == "" {
		return "", "", "", ""
	}
	name := filepath.Base(roots.branchRef)
	refFile = filepath.Join(roots.branchRefDir, name)
	logFile = filepath.Join(roots.branchLogDir, name)
	return refFile, refFile + ".lock", logFile, logFile + ".lock"
}

// gitCommonDirFiles holds the few shared bare-clone files ordinary task work
// may update. Repository-wide maintenance files (packed-refs, gc.pid, info/)
// are deliberately absent: enforce-mode agents must not mutate them.
type gitCommonDirFiles struct {
	packedRefsLock       string
	shallow, shallowLock string
	// stashRef/stashRefLock/stashLog/stashLogLock: refs/stash is a single,
	// fixed-name ref directly under refs/ (like refs/heads/<name> but with
	// no branch-name variability), repo-wide shared like remotes/tags
	// rather than per-branch — `git stash` fails closed without these.
	stashRef, stashRefLock string
	stashLog, stashLogLock string
}

// gitCommonDirSingleFiles derives the shared paths retained for ordinary
// workflows. shallow(.lock) is touched by shallow fetch/clone operations.
func gitCommonDirSingleFiles(roots gitSandboxRoots) gitCommonDirFiles {
	if roots.commonDir == "" {
		return gitCommonDirFiles{}
	}
	shallow := filepath.Join(roots.commonDir, "shallow")
	packedRefs := filepath.Join(roots.commonDir, "packed-refs")
	stashRef := filepath.Join(roots.commonDir, "refs", "stash")
	stashLog := filepath.Join(roots.commonDir, "logs", "refs", "stash")
	return gitCommonDirFiles{
		packedRefsLock: packedRefs + ".lock",
		shallow:        shallow,
		shallowLock:    shallow + ".lock",
		stashRef:       stashRef,
		stashRefLock:   stashRef + ".lock",
		stashLog:       stashLog,
		stashLogLock:   stashLog + ".lock",
	}
}

func injectSandboxGitEnv(cfg *RunConfig, roots gitSandboxRoots, overlay gitSandboxOverlay) error {
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES")
	var err error
	cfg.ExtraEnv, err = appendGitConfigEnv(cfg.ExtraEnv, "fetch.unpackLimit", "100000000")
	if err != nil {
		return err
	}
	if !sandboxUsesGitObjectOverlay() {
		if roots.objectDir != "" {
			// Trusted assignments are appended last by every provider runner, so
			// they override any ambient object-store redirection inherited by the
			// Sybra process. Darwin writes directly to the shared object store.
			cfg.ExtraEnv = append(cfg.ExtraEnv,
				"GIT_OBJECT_DIRECTORY="+roots.objectDir,
				"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
			)
		}
		return nil
	}
	if roots.objectDir == "" && overlay.objectDir == "" {
		return nil
	}
	if roots.objectDir == "" || overlay.objectDir == "" {
		return fmt.Errorf("incomplete sandbox git object paths: shared=%q overlay=%q", roots.objectDir, overlay.objectDir)
	}
	cfg.ExtraEnv = append(cfg.ExtraEnv,
		"GIT_OBJECT_DIRECTORY="+overlay.objectDir,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+roots.objectDir,
	)
	return nil
}

func appendGitConfigEnv(env []string, key, value string) ([]string, error) {
	countText := os.Getenv("GIT_CONFIG_COUNT")
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			countText = v
		}
	}
	count := 0
	if countText != "" {
		parsed, err := strconv.Atoi(countText)
		if err != nil || parsed < 0 || parsed > 1000 {
			return nil, fmt.Errorf("invalid GIT_CONFIG_COUNT %q", countText)
		}
		count = parsed
	}
	countKey := "GIT_CONFIG_KEY_" + strconv.Itoa(count)
	countValue := "GIT_CONFIG_VALUE_" + strconv.Itoa(count)
	env = stripEnvKeys(env, "GIT_CONFIG_COUNT", countKey, countValue)
	return append(env,
		"GIT_CONFIG_COUNT="+strconv.Itoa(count+1),
		countKey+"="+key,
		countValue+"="+value,
	), nil
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

// applySandboxReadMode resolves the read-visibility posture onto an
// already-built enforce spec. It mirrors the write sandbox's report/enforce
// split for the same reason: "report" resolves and logs the allowlist but
// leaves cfg.sandbox.readRoots empty, so a defective read allowlist can only
// ever affect a deployment that explicitly asked for read enforcement.
//
// An invalid value degrades to "off" rather than erroring. Failing the run
// closed on a typo would take down every agent at once, which is a strictly
// worse outcome than leaving reads at today's posture.
func (m *Manager) applySandboxReadMode(cfg *RunConfig) error {
	requested := cfg.SandboxReadMode
	if strings.TrimSpace(requested) == "" {
		m.mu.RLock()
		requested = m.defaultSandboxReadMode
		m.mu.RUnlock()
	}
	mode, err := config.NormalizeSandboxReadMode(requested)
	if err != nil {
		m.logger.Warn("agent.sandbox.read.invalid", "task_id", cfg.TaskID, "value", requested, "err", err)
		return nil
	}
	if mode == "off" {
		return nil
	}
	roots := m.resolveSandboxReadRoots(cfg)
	if mode != "enforce" {
		m.logger.Info("agent.sandbox.read.report", "task_id", cfg.TaskID, "role", string(cfg.Role), "read_roots", roots)
		return nil
	}
	profilePath, err := buildReadProfile(cfg.sandbox.profilePath, roots, cfg.sandbox.sandboxHome)
	if err != nil {
		m.logger.Error("agent.sandbox.read.failed", "task_id", cfg.TaskID, "err", err)
		return fmt.Errorf("agent.Run: sandbox read profile: %w", err)
	}
	cfg.sandbox.profilePath = profilePath
	cfg.sandbox.readRoots = roots
	m.logger.Info("agent.sandbox.read.enforce", "task_id", cfg.TaskID, "role", string(cfg.Role), "read_roots", roots)
	return nil
}

// systemReadRoots are the OS roots every provider CLI and toolchain needs to
// exec at all. /opt is deliberately absent: on the server it holds the live
// deploy checkout (/opt/sybra/src), which the #2780 trace measured as read by
// exactly zero toolchain steps, so granting it would re-open the one root
// this restriction exists to close. /nix/store is the immutable, system-wide
// runtime closure for Nix-backed host tools; granting it is equivalent to the
// fixed OS roots above and does not expose an operator checkout.
var systemReadRoots = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/var/lib", "/private/var/db",
	"/System", "/Library", "/dev", "/private/var/select", "/nix/store",
}

// hostRuntimeReadRoots are the package-manager runtime closures a host tool
// links against, in the same spirit as /nix/store above. Granting the binary
// without them is granting nothing: the dynamic loader fails before the
// process runs, which is what made every verifier command abort on a mac
// whose git came from Homebrew (#3390).

// toolchainReadSubdirs are home-relative roots that hold the language
// toolchains themselves. mise's install tree is the load-bearing one: it
// carries the Go stdlib source that every compile reads, and it never appears
// on a command line, so it is absent from any log-derived allowlist (#2780).
var toolchainReadSubdirs = []string{
	filepath.Join(".local", "share", "mise"),
	filepath.Join(".local", "state", "mise"),
	filepath.Join(".local", "bin"),
	filepath.Join(".config", "mise"),
	filepath.Join(".config", "go"),
	filepath.Join("go", "bin"),
}

var githubReadSubdirs = []string{filepath.Join(".config", "gh")}

// homeRootReadFiles sit directly in the operator home rather than under a
// grantable directory, so granting only directories silently breaks them.
// .gitconfig carries the credential helper that authenticates pushes.
var homeRootReadFiles = []string{".gitconfig"}

// Provider agents need the account state in .claude.json; repository-owned
// deterministic commands deliberately do not.
var providerHomeRootReadFiles = []string{".claude.json"}

// providerReadSubdirs hold provider executables installed outside mise's
// toolchain tree. Claude's native installer places the versioned Bun binary
// here and ~/.local/bin/claude points into it; granting only the symlink lets
// exec start but makes Bun fail immediately when it reads its own image.
var providerReadSubdirs = []string{filepath.Join(".local", "share", providerid.Claude)}

// homeStateLinks are the provider state dirs as spelled in the home
// directory. They are added *uncanonicalized*, in addition to their resolved
// targets, because on the server ~/.claude is a symlink to /data/sybra/claude:
// granting only the resolved target leaves the symlink itself absent from the
// mount namespace, so every tilde-relative lookup the CLI makes still fails.
// That failure is silent and authentication-shaped rather than an EROFS.
//
// .agents is not a provider state dir but codex's skills tree (~/.agents/skills,
// 37 reads in a single traced codex run); without it codex loses every skill.
var homeStateLinks = []string{
	".claude",
	".codex",
	".copilot",
	".agents",
	filepath.Join(".local", "share", "opencode"),
}

// resolveSandboxReadRoots returns the additional read-only roots for a run,
// on top of the write roots (which are always readable). Roots that do not
// exist on this host are skipped rather than failing the run: the list spans
// two platforms and several optional toolchains, and a fail-closed miss here
// would break every agent rather than deny one path.
//
// Roles are carved out only where the #2780 audit measured a structural need:
// monitor reads the Sybra board by design (213 reads), and a read-only Dir
// (human-review's deploy-checkout fallback) must stay readable to be
// reviewable. The orchestrator's own memory needs no entry — it lives under
// ~/.claude, already a write root.
func (m *Manager) resolveSandboxReadRoots(cfg *RunConfig) []string {
	var roots []string
	// Both the pre-resolution spelling and the resolved target are granted
	// whenever they differ. Granting only the target is a hard break: /bin,
	// /sbin, /lib and /lib64 are symlinks into /usr on the deploy host, so a
	// canonicalized-only allowlist leaves /bin absent from the mount
	// namespace and every "#!/bin/sh" shebang fails with ENOENT. The same
	// applies to ~/.claude, a symlink to /data/sybra/claude there.
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		canon, err := canonicalizeRoot(p)
		if err != nil {
			return
		}
		if canon != p {
			roots = append(roots, p)
		}
		roots = append(roots, canon)
	}
	for _, p := range systemReadRoots {
		add(p)
	}
	for _, p := range hostRuntimeReadRoots() {
		add(p)
	}
	// The mirror's links resolve into whatever store hostMiseDataDir names, so
	// a host that moves it with MISE_DATA_DIR or XDG_DATA_HOME needs that path
	// granted too — the home-relative constant below only covers the default.
	add(hostMiseDataDir())
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		for _, sub := range toolchainReadSubdirs {
			add(filepath.Join(home, sub))
		}
		if !cfg.Role.IsVerifier() {
			for _, sub := range githubReadSubdirs {
				add(filepath.Join(home, sub))
			}
			for _, f := range homeRootReadFiles {
				add(filepath.Join(home, f))
			}
		}
		if !cfg.DisableVerifierControl {
			for _, sub := range providerReadSubdirs {
				add(filepath.Join(home, sub))
			}
			for _, f := range providerHomeRootReadFiles {
				add(filepath.Join(home, f))
			}
			for _, l := range homeStateLinks {
				add(filepath.Join(home, l))
			}
		}
	}
	// A provider launcher can live outside the fixed toolchain roots. On
	// macOS, for example, Homebrew installs Codex below /opt/homebrew and puts
	// a symlink in its bin directory. Grant only the selected executable's
	// spelling and resolved target: granting /opt wholesale would expose the
	// protected server checkout below /opt/sybra, while granting neither makes
	// sandbox-exec report the existing binary as ENOENT.
	if !cfg.DisableVerifierControl && cfg.provider != nil {
		for _, root := range providerExecutableReadRoots(cfg.provider.Name()) {
			add(root)
		}
	}
	add(cfg.sandbox.readOnlyDir)
	for _, p := range cfg.ReadOnlyPaths {
		add(p)
	}
	if cfg.Role == RoleMonitor {
		add(config.HomeDir())
	}
	// Every write root must also be readable. On Linux a later rw bind would
	// cover this anyway, but darwin's seatbelt evaluates file-read* and
	// file-write* independently, so the set has to be explicit to work on
	// both platforms from one resolved list.
	//
	// Routed through add() rather than appended raw: seatbelt matches
	// subpaths literally, so an unresolved /var/... root would not match the
	// /private/var/... path the process actually opens on darwin.
	for _, p := range cfg.sandbox.writeRoots() {
		add(p)
	}
	return dedupeRoots(roots...)
}

// providerExecutableReadRoots returns the narrow external installation roots
// needed to start providerName. The launcher itself is always included; add()
// grants both its spelling and its resolved target. Codex also ships a sibling
// code-mode host that it execs after startup, so grant that exact executable
// when present. An npm launcher needs its package's adjacent metadata and
// optional platform package, so grant the containing vendor scope (for example
// @openai) or unscoped package only.
func providerExecutableReadRoots(providerName string) []string {
	executable, err := exec.LookPath(providerName)
	if err != nil {
		return nil
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil
	}
	roots := []string{executable}
	resolved, err := canonicalizeRoot(executable)
	if err != nil {
		return roots
	}
	if packageRoot := nodeModulesProviderRoot(resolved); packageRoot != "" {
		roots = append(roots, packageRoot)
	}
	if providerName == providerid.Codex {
		companion := filepath.Join(filepath.Dir(resolved), "codex-code-mode-host")
		// The trusted Codex distributions ship the host as a regular sibling.
		// Do not follow an adjacent symlink: add() canonicalizes every root, so
		// accepting one here could grant its target outside the installation.
		if info, statErr := os.Lstat(companion); statErr == nil && info.Mode().IsRegular() {
			roots = append(roots, companion)
		}
	}
	return roots
}

func nodeModulesProviderRoot(path string) string {
	marker := string(filepath.Separator) + "node_modules" + string(filepath.Separator)
	idx := strings.LastIndex(path, marker)
	if idx < 0 {
		return ""
	}
	prefix := path[:idx+len(marker)]
	rest := path[idx+len(marker):]
	parts := strings.Split(rest, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	if strings.HasPrefix(parts[0], "@") {
		if len(parts) < 2 || parts[1] == "" {
			return ""
		}
		return filepath.Clean(prefix + parts[0])
	}
	return filepath.Clean(prefix + parts[0])
}

// providerAppSupportRoot returns the macOS per-user application-data root, or
// "" where it does not exist (Linux, or a home that has none).
//
// Granted because the codex CLI's in-process app-server creates a directory
// under it at startup. Narrower grants were measured and do not work: naming
// a codex-specific subpath still fails, because creating that subdirectory
// requires write on the parent.
// claudeScratchRoot returns the Claude Code per-user scratchpad root, or ""
// when it cannot be established safely.
//
// Claude Code writes its per-session working files under /tmp/claude-<uid>.
// On darwin that is not covered by the tmp root: os.TempDir() resolves to
// $TMPDIR (/var/folders/.../T) while /tmp resolves to /private/tmp, so the
// scratchpad is invisible to the sandbox and every write there fails EPERM.
// Measured: an agent retried the same denied mkdir eight times in thirty
// seconds rather than progressing.
//
// Resolves /tmp explicitly rather than os.TempDir(): the location Claude Code
// uses is /tmp, so deriving it from $TMPDIR would grant a
// /var/folders/.../claude-<uid> directory if one happened to exist and leave
// the real scratchpad denied.
//
// Refuses a symlink. /tmp is world-writable, so any local process can
// pre-create /tmp/claude-<uid> pointing anywhere; canonicalizing that would
// hand the agent a writable root outside /tmp — defeating the boundary this
// sandbox exists to enforce. Bailing out costs the scratchpad grant, which
// degrades to the EPERM this fixes, rather than silently widening the
// sandbox.
func claudeScratchRoot() string {
	return resolveScratchRoot(filepath.Join("/tmp", fmt.Sprintf("claude-%d", os.Getuid())))
}

// resolveScratchRoot validates and canonicalizes one scratchpad path. Split
// from claudeScratchRoot so the symlink refusal is testable without touching
// the real /tmp/claude-<uid> on a developer machine.
func resolveScratchRoot(root string) string {
	if fi, err := os.Lstat(root); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return ""
		}
	} else if !os.IsNotExist(err) {
		return ""
	}
	canon, err := canonicalizeCreatedRoot(root, 0o700)
	if err != nil {
		return ""
	}
	// canonicalizeCreatedRoot resolves symlinks. Accept only the path itself
	// or darwin's /private-prefixed alias of it, so a link that slipped in
	// between the Lstat and here cannot widen the grant.
	if canon != root && canon != filepath.Join("/private", root) {
		return ""
	}
	return canon
}

func providerAppSupportRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	root := filepath.Join(home, "Library", "Application Support")
	canon, err := canonicalizeRoot(root)
	if err != nil {
		return ""
	}
	return canon
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
		sessionReadOnly:        cfg.ReadOnlyDir,
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
	if m.maxInFlightPerProvider > 0 && a.Provider != "" && m.liveByProvider[a.Provider] >= m.maxInFlightPerProvider {
		m.mu.Unlock()
		cancel()
		return fmt.Errorf("%w: %s (%d)", ErrProviderCapacityReached, a.Provider, m.maxInFlightPerProvider)
	}
	if cfg.capacityReservation != nil && cfg.capacityReservation.manager == m {
		reserved = cfg.capacityReservation.consumeLocked()
	}
	if !reserved && !admitClass(class, m.liveByClass, m.reservedByClass, m.classFloors, m.maxConcurrent) {
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
	if a.done != nil {
		metrics.AgentClassAdmitted(string(class))
		if borrowed {
			metrics.AgentClassBorrowed(string(class))
		}
	}
	return nil
}

func (m *Manager) startAgentRunner(ctx context.Context, a *Agent, cfg RunConfig, prov Provider, cancel context.CancelFunc) error {
	if cfg.Mode != "headless" {
		cancel()
		return fmt.Errorf("unknown mode: %s", cfg.Mode)
	}
	m.mu.RLock()
	backend := m.executionBackend
	m.mu.RUnlock()
	if backend == nil {
		cancel()
		return errors.New("execution backend is not configured")
	}
	lastEmit := time.Time{}
	sink := &managerExecutionSink{manager: m, agent: a, outputStart: len(a.Output()), lastEmit: &lastEmit}
	start := ExecutionStart{
		Spec:   executionSpecFromAgent(a),
		Config: cfg, Provider: prov, Sink: sink,
		stop:    func(context.Context) error { m.stopLocalAgent(a); return nil },
		steer:   func(text string) error { return m.sendHeadlessSteerMessage(a, text) },
		approve: m.respondExecutionApproval,
		inspect: func() ExecutionInspection {
			snapshot := a.View()
			return ExecutionInspection{State: string(snapshot.State), Command: snapshot.Command, Agent: snapshot}
		},
	}
	// Keep the local bridge available to a routing backend so its durable
	// scheduler can explicitly select local fallback for this individual run.
	start.runExisting = func(runCtx context.Context, eventSink ExecutionEventSink, handle ExecutionHandle) {
		a.setExecutionSink(eventSink, handle)
		m.runHeadless(runCtx, a, cfg)
	}
	handle, err := backend.Start(ctx, start)
	if err != nil {
		cancel()
		return err
	}
	sink.bind(ctx, backend, handle)
	m.mu.Lock()
	if a.GetState().IsTerminal() {
		// A sink-driven backend may complete before Start returns. markAgentDone
		// already released the run; do not resurrect a stale control handle.
	} else {
		m.activeExecutions[a.ID] = activeExecution{
			backend: backend, handle: handle, outputStart: sink.outputStart,
			lastEmit: sink.lastEmit, sink: sink,
		}
		m.executionAgents[handle] = a.ID
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) markAgentDone(ctx context.Context, a *Agent) {
	if a == nil || a.done == nil {
		return
	}
	a.doneOnce.Do(func() {
		close(a.done)
		m.completeAttempt(ctx, a, attemptTerminalOutcome(a))
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
		if execution, ok := m.activeExecutions[a.ID]; ok {
			delete(m.executionAgents, execution.handle)
		}
		delete(m.activeExecutions, a.ID)
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
// Absent paths are materialized rather than skipped. Skipping them looks
// harmless — there is nothing there to protect — but the enclosing directory
// is writable, so a run could simply create settings.json and persist hooks
// that way. An empty settings file and an empty hooks dir are both no-ops to
// the CLI, and both are then bindable.
func claudeDurableConfigPaths(stateRoot string) []string {
	if strings.TrimSpace(stateRoot) == "" {
		return nil
	}
	var out []string
	for _, f := range []struct {
		name string
		dir  bool
		seed string
	}{
		{name: "settings.json", seed: "{}\n"},
		{name: "settings.local.json", seed: "{}\n"},
		{name: "hooks", dir: true},
	} {
		p := filepath.Join(stateRoot, f.name)
		if err := materializeDenyTarget(p, f.dir, f.seed); err != nil {
			// Only skip what cannot be created: binding a nonexistent path
			// fails the spawn, and failing the run closed over a config file
			// is worse than leaving that one path writable.
			continue
		}
		out = append(out, p)
	}
	return out
}

// materializeDenyTarget makes path exist so it can be bound read-only,
// leaving any existing content untouched.
func materializeDenyTarget(path string, dir bool, seed string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if dir {
		return os.MkdirAll(path, 0o700)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(seed), 0o600)
}

// recordToolCall writes one ledger record, best-effort. A ledger write must
// never disturb a run: this sits on the stream path, and losing an
// observation is strictly preferable to failing the agent that made it.
func (m *Manager) recordToolCall(r toolledger.Record) {
	if m == nil {
		return
	}
	m.mu.RLock()
	ledger := m.toolLedger
	m.mu.RUnlock()
	// Checked here, not left to a nil-receiver guard inside the store: the
	// field is an interface, so an unset ledger is a nil interface and calling
	// through it panics — on the stream path, taking the whole run with it.
	if ledger == nil {
		return
	}
	if err := ledger.Log(r); err != nil {
		m.logger.Warn("agent.tool_ledger.write_failed", "agent_id", r.AgentID, "tool", r.Tool, "err", err)
	}
}
