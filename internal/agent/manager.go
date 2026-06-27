package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/provider"
)

type EmitFunc func(event string, data any)

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

	// reg persists live-agent records so subprocesses can be reattached
	// after an app restart. nil disables survival (legacy behaviour).
	reg            *registryStore
	surviveRestart bool

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

func NewManager(ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string) *Manager {
	return &Manager{
		agents:         make(map[string]*Agent),
		dispatchClaims: make(map[string]struct{}),
		ctx:            ctx,
		emit:           emit,
		logger:         logger,
		logDir:         logDir,
		defaultProv:    "claude",
	}
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

func (m *Manager) SetOnComplete(fn func(ag *Agent)) {
	m.onComplete = fn
}

// EnableSurviveRestart turns on restart survival: headless (and, once
// Phase 3 lands, interactive) subprocesses are detached and recorded in a
// registry under dir so the next app instance can reattach to them. Call
// once during startup before ReattachAll.
func (m *Manager) EnableSurviveRestart(dir string) error {
	s, err := newRegistryStore(dir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.reg = s
	m.surviveRestart = true
	m.mu.Unlock()
	return nil
}

// SetSessionSink installs the callback used to persist a crashed agent's
// session id into its task's AgentRun during dead-reattach. Set once at
// startup before ReattachAll.
func (m *Manager) SetSessionSink(fn func(taskID, agentID, sessionID string) error) {
	m.mu.Lock()
	m.sessionSink = fn
	m.mu.Unlock()
}

func (m *Manager) sessionSinkFn() func(taskID, agentID, sessionID string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionSink
}

// SetTaskExists installs the callback used to skip recreating a codex agent
// whose task was deleted. Set once at startup before ReattachAll.
func (m *Manager) SetTaskExists(fn func(taskID string) bool) {
	m.mu.Lock()
	m.taskExists = fn
	m.mu.Unlock()
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

// registry returns the registry store (nil when survival is disabled).
func (m *Manager) registry() *registryStore {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reg
}

// saveRegistry snapshots the agent to disk. No-op when survival is off.
func (m *Manager) saveRegistry(a *Agent) {
	reg := m.registry()
	if reg == nil || a == nil {
		return
	}
	a.mu.RLock()
	rec := Record{
		ID:                 a.ID,
		TaskID:             a.TaskID,
		Name:               a.Name,
		Mode:               a.Mode,
		Provider:           a.Provider,
		Model:              a.Model,
		PID:                a.PID,
		SessionID:          a.SessionID,
		LogPath:            a.LogPath,
		CWD:                a.sessionCWD,
		StartedAt:          a.StartedAt,
		StdinPath:          a.stdinPath,
		MaxTurns:           a.MaxTurns,
		RequirePermissions: a.requirePermissions,
		ReasoningEffort:    a.ReasoningEffort,
	}
	a.mu.RUnlock()
	rec.ProcStartedAt = processStartString(rec.PID)
	if err := reg.Save(rec); err != nil {
		m.logger.Warn("agent.registry.save", "id", a.ID, "err", err)
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

// SetApprovalAddr sets the HTTP address for the tool approval server.
func (m *Manager) SetApprovalAddr(addr string) {
	m.approvalAddr = addr
}

// SetMaxConcurrent sets the maximum number of concurrently running agents.
// A value of 0 means unlimited.
func (m *Manager) SetMaxConcurrent(n int) {
	m.mu.Lock()
	m.maxConcurrent = n
	m.mu.Unlock()
}

func (m *Manager) SetDefaultProvider(name string) {
	m.mu.Lock()
	m.defaultProv = normalizeProvider(name)
	m.mu.Unlock()
}

// SetHealthGate wires in a provider health checker so Run() can refuse or
// failover when the requested provider is unhealthy. A nil gate disables the
// check entirely (tests, feature-disabled mode).
func (m *Manager) SetHealthGate(g provider.HealthGate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

func (m *Manager) SetLimitGate(g LimitGate, policy limits.Policy) {
	m.mu.Lock()
	m.limitGate = g
	m.limitPolicy = policy
	m.mu.Unlock()
}

func (m *Manager) LimitPolicy() limits.Policy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.limitPolicy
}

func (m *Manager) SetLimitSink(fn func(limits.Snapshot)) {
	m.mu.Lock()
	m.limitSink = fn
	m.mu.Unlock()
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

// SetBashTimeoutMs sets the default --bashTimeoutMs passed to claude -p.
// Zero disables the flag (no per-call timeout).
func (m *Manager) SetBashTimeoutMs(ms int) {
	m.mu.Lock()
	m.bashTimeoutMs = ms
	m.mu.Unlock()
}

// SetRetryWatchdog sets the default CLAUDE_CODE_RETRY_WATCHDOG value injected
// into headless claude subprocess environments. Zero resets to "not set"
// (no env var exported).
func (m *Manager) SetRetryWatchdog(n int) {
	m.mu.Lock()
	m.retryWatchdog = n
	m.mu.Unlock()
}

// SetFallbackModel sets the default --fallback-model passed to headless claude
// runs. Empty string clears the fallback (no flag added).
func (m *Manager) SetFallbackModel(model string) {
	m.mu.Lock()
	m.fallbackModel = model
	m.mu.Unlock()
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
