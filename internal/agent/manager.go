package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	gate          provider.HealthGate
}

func NewManager(ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string) *Manager {
	return &Manager{
		agents:      make(map[string]*Agent),
		ctx:         ctx,
		emit:        emit,
		logger:      logger,
		logDir:      logDir,
		defaultProv: "claude",
	}
}

func (m *Manager) SetOnComplete(fn func(ag *Agent)) {
	m.onComplete = fn
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

// recordCompletion is called from every runner's terminal site just before
// onComplete fires. Records duration + result into the metrics pipeline.
func (m *Manager) recordCompletion(a *Agent, ok bool) {
	dur := time.Since(a.StartedAt)
	result := "ok"
	if !ok {
		result = "error"
	}
	metrics.AgentCompleted(result, dur)
}

// RunningCount returns the number of currently running agents.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveCount
}
