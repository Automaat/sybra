package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/provider"
)

type EmitFunc func(event string, data any)

// ErrSurvivalRegistry marks failures initializing restart-survival persistence.
var ErrSurvivalRegistry = errors.New("agent survival registry")

// Guardrails defines per-agent execution limits.
type Guardrails struct {
	MaxCostUSD float64
	MaxTurns   int
	// TurnCostFraction is the fraction of MaxCostUSD below which a turns
	// escalation is auto-continued without human approval. Default 0.8.
	// Only effective when MaxCostUSD > 0.
	TurnCostFraction float64
	// TurnMultiplier scales the turn limit on each auto-continuation so
	// the agent gets progressively more turns. Default 2.
	TurnMultiplier float64
}

type Manager struct {
	agents        map[string]*Agent
	mu            sync.RWMutex
	liveCount     int
	ctx           context.Context
	emit          EmitFunc
	onComplete    func(ag *Agent)
	logger        *slog.Logger
	logDir        string
	maxConcurrent int
	defaultProv   string
	approvalAddr  string // localhost:port for the HTTP tool approval server
	guardrails    Guardrails
	bashTimeoutMs int
	retryWatchdog int
	fallbackModel string
	gate          provider.HealthGate
	limitGate     LimitGate
	limitPolicy   limits.Policy
	limitSink     func(limits.Snapshot)

	// liveByProvider tracks in-flight agent counts per provider, incremented
	// and decremented in lockstep with liveCount (registerRunningAgent,
	// markAgentDone, and the three ReattachAll restart paths) so
	// sum(liveByProvider) == liveCount always holds. Read by gateProvider to
	// steer dispatch away from an at-cap provider.
	liveByProvider map[string]int
	// maxInFlightPerProvider caps concurrent in-flight agents per provider.
	// 0 disables the cap (soft cap: never blocks dispatch, only redirects).
	maxInFlightPerProvider int
	// dispatchJitterMs bounds a uniform random delay applied before headless
	// dispatch to de-correlate a wave of same-tick starts. 0 disables jitter.
	dispatchJitterMs int

	// reg persists live-agent records so subprocesses can be reattached
	// after an app restart. nil disables survival (legacy behaviour).
	// Manager.mu guards only this pointer and survival config; registryStore
	// owns serialization of its on-disk Save/List/Delete operations.
	reg               survivalRegistry
	surviveRestart    bool
	surviveRestartDir string

	// sessionSink, when set, persists a crashed agent's captured session id
	// to its task's AgentRun on dead-reattach, so restart-stale recovery can
	// resume the conversation via --resume instead of cold-restarting. A
	// non-nil error means persistence failed and the registry record should
	// be retained for a later retry.
	sessionSink func(taskID, agentID, sessionID string) error

	// taskExists, when set, reports whether a task still exists. Used to
	// avoid recreating a zombie codex agent whose chat task was deleted.
	taskExists func(taskID string) bool

	// dispatchClaims serializes agent dispatch per task. A claim is held for
	// the full duration of a StartAgent call — across the (multi-second)
	// worktree-preparation window during which the agent is not yet registered
	// in `agents`. Without it, two independent dispatchers (the workflow
	// cascade and the recovery loop) can both observe "no running agent" and
	// each start an agent on the same worktree. This is a pure in-flight lock:
	// it intentionally does NOT inspect running agents, so a step transition
	// dispatching its next agent inside the prior agent's onComplete (whose
	// `done` channel is not yet closed) is never blocked.
	dispatchClaims map[string]struct{}
}

type LimitGate interface {
	ProviderAvailable(provider string, policy limits.Policy) (bool, string)
	ChooseProvider(requested string, candidates []string, healthy func(string) bool, policy limits.Policy) (string, string)
}

// LimitGateOrNil wraps store as a LimitGate, returning a genuine nil
// interface when store is nil. Assigning a nil *limits.Store directly to a
// LimitGate field produces a non-nil interface holding a nil pointer, which
// defeats `== nil` guards and panics on the first method call.
func LimitGateOrNil(store *limits.Store) LimitGate {
	if store == nil {
		return nil
	}
	return store
}

// ManagerConfig contains startup-only wiring. Values that are intentionally
// live-editable are grouped in Runtime and updated via ReplaceRuntimeConfig.
type ManagerConfig struct {
	Runtime ManagerRuntimeConfig

	OnComplete        func(ag *Agent)
	ApprovalAddr      string
	SurviveRestartDir string
	SessionSink       func(taskID, agentID, sessionID string) error
	TaskExists        func(taskID string) bool
	LimitSink         func(limits.Snapshot)
}

// ManagerRuntimeConfig holds settings that affect future runs and may change
// on config reload without rebuilding the manager.
type ManagerRuntimeConfig struct {
	MaxConcurrent   int
	DefaultProvider string
	BashTimeoutMs   int
	RetryWatchdog   int
	FallbackModel   string
	LimitGate       LimitGate
	LimitPolicy     limits.Policy
	// MaxInFlightPerProvider caps concurrent in-flight agents per provider.
	// 0 disables the cap.
	MaxInFlightPerProvider int
	// DispatchJitterMs bounds a uniform random delay applied before headless
	// dispatch. 0 disables jitter.
	DispatchJitterMs int
}

func NewManager(ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string, cfg ManagerConfig) (*Manager, error) {
	defaultProv, err := normalizeProviderName(cfg.Runtime.DefaultProvider)
	if err != nil {
		return nil, fmt.Errorf("default provider: %w", err)
	}
	m := &Manager{
		agents:                 make(map[string]*Agent),
		dispatchClaims:         make(map[string]struct{}),
		liveByProvider:         make(map[string]int),
		ctx:                    ctx,
		emit:                   emit,
		onComplete:             cfg.OnComplete,
		logger:                 logger,
		logDir:                 logDir,
		approvalAddr:           cfg.ApprovalAddr,
		defaultProv:            defaultProv,
		maxConcurrent:          cfg.Runtime.MaxConcurrent,
		bashTimeoutMs:          cfg.Runtime.BashTimeoutMs,
		retryWatchdog:          cfg.Runtime.RetryWatchdog,
		fallbackModel:          cfg.Runtime.FallbackModel,
		limitGate:              cfg.Runtime.LimitGate,
		limitPolicy:            copyLimitPolicy(cfg.Runtime.LimitPolicy),
		limitSink:              cfg.LimitSink,
		sessionSink:            cfg.SessionSink,
		taskExists:             cfg.TaskExists,
		maxInFlightPerProvider: cfg.Runtime.MaxInFlightPerProvider,
		dispatchJitterMs:       cfg.Runtime.DispatchJitterMs,
	}
	if cfg.SurviveRestartDir != "" {
		s, err := newRegistryStore(cfg.SurviveRestartDir)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSurvivalRegistry, err)
		}
		m.reg = s
		m.surviveRestart = true
		m.surviveRestartDir = cfg.SurviveRestartDir
	}
	return m, nil
}

// ClaimTaskDispatch reserves the right to dispatch an agent for taskID,
// returning false when a dispatch is already in flight for the same task. The
// caller MUST NOT start an agent on a false return, and MUST ReleaseTaskDispatch
// once dispatch finishes (success or failure) on a true return. This closes the
// window between dispatch start and agent registration where a concurrent
// dispatcher would otherwise see no running agent and start a duplicate.
func (m *Manager) ClaimTaskDispatch(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.dispatchClaims[taskID]; held {
		return false
	}
	if m.dispatchClaims == nil {
		m.dispatchClaims = make(map[string]struct{})
	}
	m.dispatchClaims[taskID] = struct{}{}
	return true
}

// ReleaseTaskDispatch releases a claim acquired by ClaimTaskDispatch. Safe to
// call for a task with no outstanding claim.
func (m *Manager) ReleaseTaskDispatch(taskID string) {
	m.mu.Lock()
	delete(m.dispatchClaims, taskID)
	m.mu.Unlock()
}

// ReplaceRuntimeConfig replaces the complete live runtime snapshot. Settings
// affect future Run calls and config reloads without mutating startup-only callbacks.
func (m *Manager) ReplaceRuntimeConfig(cfg ManagerRuntimeConfig) error {
	defaultProv, err := normalizeProviderName(cfg.DefaultProvider)
	if err != nil {
		return fmt.Errorf("default provider: %w", err)
	}
	m.mu.Lock()
	m.maxConcurrent = cfg.MaxConcurrent
	m.defaultProv = defaultProv
	m.bashTimeoutMs = cfg.BashTimeoutMs
	m.retryWatchdog = cfg.RetryWatchdog
	m.fallbackModel = cfg.FallbackModel
	m.limitGate = cfg.LimitGate
	m.limitPolicy = copyLimitPolicy(cfg.LimitPolicy)
	m.maxInFlightPerProvider = cfg.MaxInFlightPerProvider
	m.dispatchJitterMs = cfg.DispatchJitterMs
	m.mu.Unlock()
	return nil
}

// InFlightByProvider returns a snapshot of in-flight agent counts by
// provider, kept in lockstep with RunningCount's liveCount.
func (m *Manager) InFlightByProvider() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.liveByProvider)
}

func (m *Manager) sessionSinkFn() func(taskID, agentID, sessionID string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionSink
}

func (m *Manager) taskExistsFn() func(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.taskExists
}

// survives reports whether restart survival is active.
func (m *Manager) survives() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.surviveRestart && m.reg != nil
}

// willDetach reports whether a run survives a restart. Headless and
// interactive Claude survive as detached processes (Claude one-shot passes
// its prompt as an argument, no stdin). Interactive codex "survives" by
// recreate-on-restart: it has no persistent process between its independent
// per-turn `codex exec` invocations, so the agent record persists and the
// idle agent is rebuilt on the next startup. Single source of truth
// mirrored by the runner branches.
func willDetach(cfg RunConfig) bool {
	switch cfg.Mode {
	case "headless", "interactive":
		return true
	default:
		return false
	}
}

// registry returns the survival registry (nil when survival is disabled).
func (m *Manager) registry() survivalRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reg
}

func (m *Manager) registryDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.surviveRestartDir
}

// saveRegistry snapshots the agent to disk. No-op when survival is off.
func (m *Manager) saveRegistry(a *Agent) {
	reg := m.registry()
	if reg == nil || a == nil {
		return
	}
	rec := a.toRecord()
	rec.ProcStartedAt = processStartString(rec.PID)
	if err := reg.Save(rec); err != nil {
		m.logger.Warn(
			"agent.registry.save",
			"id", rec.ID,
			"task_id", rec.TaskID,
			"mode", rec.Mode,
			"pid", rec.PID,
			"log_path", rec.LogPath,
			"err", err,
		)
	}
}

// signalKill terminates an agent's subprocess. Prefers the *exec.Cmd
// handle (live this lifetime); falls back to the PID for reattached
// agents that have no handle.
func (m *Manager) signalKill(a *Agent) {
	if cmd := a.GetCmd(); cmd != nil && cmd.Process != nil {
		stopWithSIGINT(cmd, a.done, stopSIGINTGrace)
		return
	}
	signalPID(a.GetPID(), stopSIGINTGrace)
}

// SetHealthGate wires in a provider health checker so Run() can refuse or
// failover when the requested provider is unhealthy. A nil gate disables the
// check entirely (tests, feature-disabled mode).
func (m *Manager) SetHealthGate(g provider.HealthGate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

func (m *Manager) LimitPolicy() limits.Policy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return copyLimitPolicy(m.limitPolicy)
}

func copyLimitPolicy(policy limits.Policy) limits.Policy {
	if policy.SubscriptionMonthlyUSD != nil {
		policy.SubscriptionMonthlyUSD = copyStringFloatMap(policy.SubscriptionMonthlyUSD)
	}
	if policy.ProviderEnabled != nil {
		policy.ProviderEnabled = copyStringBoolMap(policy.ProviderEnabled)
	}
	return policy
}

func copyStringFloatMap(in map[string]float64) map[string]float64 {
	return maps.Clone(in)
}

func copyStringBoolMap(in map[string]bool) map[string]bool {
	return maps.Clone(in)
}

// ProviderRateLimited reports whether the named provider is currently in a
// rate-limit cooldown — distinct from logged-out / auth failure. A nil health
// gate (checks disabled, tests) reports false. An empty name resolves to the
// default provider. Recovery loops consult this to wait out a transient rate
// limit; auth failures deliberately do NOT count here so they keep taking the
// human-required path (a human must log in) instead of waiting indefinitely.
func (m *Manager) ProviderRateLimited(name string) bool {
	m.mu.RLock()
	g := m.gate
	if name == "" {
		name = m.defaultProv
	}
	m.mu.RUnlock()
	if g == nil {
		return false
	}
	return g.RateLimited(name)
}

// ProviderCanFailover reports whether the health/limit gates can route work
// away from the named provider right now.
func (m *Manager) ProviderCanFailover(name string) bool {
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	if name == "" {
		name = m.defaultProv
	}
	m.mu.RUnlock()
	prov, err := lookupProvider(name)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("agent.failover.provider", "provider", name, "err", err)
		}
		return false
	}
	resolved := prov.Name()
	healthy := func(p string) bool {
		return g == nil || g.IsHealthy(p)
	}
	if g != nil && g.Failover(resolved) != "" {
		return true
	}
	if lg == nil {
		return false
	}
	alt, _ := lg.ChooseProvider(resolved, []string{"claude", "codex", "copilot"}, healthy, lp)
	return alt != ""
}

// ReportProviderSignal forwards a runner-side passive signal (rate-limit or
// auth failure) to the health gate. Safe to call with a nil gate.
func (m *Manager) ReportProviderSignal(name string, sig provider.Signal, reason string, retryAfter time.Duration) {
	m.mu.RLock()
	g := m.gate
	m.mu.RUnlock()
	if g == nil {
		return
	}
	switch sig {
	case provider.SignalAuthFailure:
		g.ReportAuthFailure(name, reason)
	case provider.SignalRateLimit:
		g.ReportRateLimit(name, retryAfter, reason)
	case provider.SignalNone:
		// no-op: caller decided not to escalate this run.
	}
}

// DefaultProvider returns the current default provider name.
func (m *Manager) DefaultProvider() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultProv
}

// SetGuardrails configures cost and turn limits applied to all agents.
func (m *Manager) SetGuardrails(g Guardrails) {
	m.mu.Lock()
	m.guardrails = g
	m.mu.Unlock()
}

// Guardrails returns the current guardrail limits.
func (m *Manager) Guardrails() Guardrails {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.guardrails
}

// canAutoContinueTurns returns true when the agent's current cost is below
// TurnCostFraction * MaxCostUSD, meaning there is still meaningful budget left
// and the turns limit can be auto-bumped without human approval.
// If MaxCostUSD == 0, auto-continue is always allowed (cost is unlimited).
func (m *Manager) canAutoContinueTurns(a *Agent) bool {
	m.mu.RLock()
	maxCost := m.guardrails.MaxCostUSD
	fraction := m.guardrails.TurnCostFraction
	m.mu.RUnlock()
	if maxCost == 0 {
		return true
	}
	if fraction <= 0 {
		fraction = 0.8
	}
	return a.GetCostUSD() < maxCost*fraction
}

// effectiveTurnMultiplier returns the configured TurnMultiplier, defaulting to 2.
func (m *Manager) effectiveTurnMultiplier() float64 {
	m.mu.RLock()
	v := m.guardrails.TurnMultiplier
	m.mu.RUnlock()
	if v <= 0 {
		return 2
	}
	return v
}

// RespondEscalation sends a human decision to a paused agent.
// continueRun=true lets the agent keep running; false kills it.
func (m *Manager) RespondEscalation(agentID string, continueRun bool) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.escalationCh == nil {
		return fmt.Errorf("agent %s has no pending escalation", agentID)
	}
	select {
	case a.escalationCh <- continueRun:
	default:
		return fmt.Errorf("agent %s escalation channel full or closed", agentID)
	}
	return nil
}

// recordCompletion records duration + result into the metrics pipeline.
// Call through fireComplete — do not call directly from runner terminal sites.
func (m *Manager) recordCompletion(a *Agent, ok bool) {
	dur := time.Since(a.StartedAt)
	result := "ok"
	if !ok {
		result = "error"
	}
	metrics.AgentCompleted(result, dur)
}

// fireComplete records completion metrics and fires onComplete exactly once
// per agent. The guard prevents a second runner goroutine (e.g.
// runner_convo_survive whose tail is still live when runner_convo exits) from
// calling onComplete a second time and double-advancing the workflow.
func (m *Manager) fireComplete(a *Agent, ok bool) {
	a.completedOnce.Do(func() {
		m.recordCompletion(a, ok)
		if m.onComplete != nil {
			m.onComplete(a)
		}
	})
}

// RunningCount returns the number of currently running agents.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveCount
}
