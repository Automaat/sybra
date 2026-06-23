package sybra

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
)

// orchestratorAgentName is the stable Name assigned to the orchestrator agent
// so the frontend and tests can identify it in agent listings.
const orchestratorAgentName = "orchestrator"

// orchestratorKickoffPrompt is the first user message delivered to the
// orchestrator brain so it actually takes a turn. The conversational runner
// only sends an initial message when RunConfig.Prompt is non-empty; without it
// the agent is parked in StatePaused, idle on stdin, and never bootstraps its
// monitor loop or dispatches work (it just sits silent forever).
const orchestratorKickoffPrompt = "Start your orchestrator session now. Follow your " +
	"operating instructions in CLAUDE.md: first ensure your recurring monitor loop is " +
	"scheduled via CronCreate(schedule=\"*/5 * * * *\", prompt=\"/sybra-monitor\") if it is " +
	"not already, then run one /sybra-monitor cycle to triage and dispatch ready work."

// orchestratorReplaceable reports whether an existing orchestrator agent should
// be reaped and replaced rather than treated as "already running". A crashed
// agent is StateStopped. A conversational agent that was started without a
// kickoff prompt parks in StatePaused with no session id and idles on stdin
// forever — that is the wedged-brain failure mode, so it is replaceable. A
// paused-between-turns agent that already established a session is healthy and
// must be left alone (otherwise the 1-minute auto-start loop would churn it).
func orchestratorReplaceable(a *agent.Agent) bool {
	switch a.GetState() {
	case agent.StateStopped:
		return true
	case agent.StatePaused:
		return a.GetSessionID() == ""
	default:
		return false
	}
}

// OrchestratorService exposes orchestrator session operations as Wails-bound methods.
type OrchestratorService struct {
	agents *agent.Manager
	audit  *audit.Logger
	logger *slog.Logger
	emit   func(string, any)

	mu      sync.Mutex
	agentID string
}

// StartOrchestrator launches the orchestrator as an in-app conversational
// Claude agent rooted at ~/.sybra (where the brain CLAUDE.md + skills live).
// The orchestrator bootstraps its own monitor loop via CronCreate on first
// turn, as instructed by orchestrator/CLAUDE.md. Provider is pinned to claude
// because /sybra-monitor is a Claude-only skill.
func (s *OrchestratorService) StartOrchestrator() error {
	// Hold the lock across agents.Run so two concurrent callers (the 1-minute
	// auto-start loop and the UI Start button) cannot both pass the replaceable
	// guard and leak an orphan brain that takes a real turn. Start is rare and
	// off any hot path, so holding the lock over the bounded process spawn is
	// acceptable.
	s.mu.Lock()
	defer s.mu.Unlock()

	if id := s.agentID; id != "" {
		if a, err := s.agents.GetAgent(id); err == nil && !orchestratorReplaceable(a) {
			return conflictError("orchestrator already running")
		}
		// Existing agent is stopped, or wedged-paused with no session — reap
		// it so a freshly kicked-off orchestrator can replace it. StopAgent is
		// idempotent on an already-terminal agent.
		_ = s.agents.StopAgent(id)
		s.agentID = ""
	}

	a, err := s.agents.Run(agent.RunConfig{
		Name:                   orchestratorAgentName,
		Mode:                   "interactive",
		Dir:                    config.HomeDir(),
		Provider:               "claude",
		Prompt:                 orchestratorKickoffPrompt,
		IgnoreConcurrencyLimit: true,
	})
	if err != nil {
		return fmt.Errorf("start orchestrator agent: %w", err)
	}
	s.agentID = a.ID

	s.logger.Info("orchestrator.started", "agent_id", a.ID)
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{Type: audit.EventOrchestratorStart})
	}
	s.emit(events.OrchestratorState, "running")
	return nil
}

// StopOrchestrator cancels the orchestrator agent's context which unwinds
// the conversational runner and closes the child claude process.
func (s *OrchestratorService) StopOrchestrator() error {
	s.mu.Lock()
	id := s.agentID
	s.agentID = ""
	s.mu.Unlock()

	if id == "" {
		return conflictError("orchestrator not running")
	}
	if err := s.agents.StopAgent(id); err != nil {
		return fmt.Errorf("stop orchestrator: %w", err)
	}
	s.logger.Info("orchestrator.stopped", "agent_id", id)
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{Type: audit.EventOrchestratorStop})
	}
	s.emit(events.OrchestratorState, "stopped")
	return nil
}

// IsOrchestratorRunning reports whether an orchestrator agent is currently alive.
func (s *OrchestratorService) IsOrchestratorRunning() bool {
	s.mu.Lock()
	id := s.agentID
	s.mu.Unlock()
	if id == "" {
		return false
	}
	a, err := s.agents.GetAgent(id)
	if err != nil {
		return false
	}
	// A wedged-paused or stopped orchestrator is not actually doing work, so
	// it does not count as running — this lets maybeStartOrchestrator replace
	// it instead of trusting a stale in-memory state forever.
	return !orchestratorReplaceable(a)
}

// GetOrchestratorAgentID returns the current orchestrator agent id, or empty
// if none is running. The frontend uses this to subscribe to agent:convo:<id>
// events for live streaming.
func (s *OrchestratorService) GetOrchestratorAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}
