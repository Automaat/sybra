package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
)

// orchestratorAgentName is the stable Name assigned to the orchestrator agent
// so the frontend and tests can identify it in agent listings.
const orchestratorAgentName = "orchestrator"

const orchestratorRole = "orchestrator"

// orchestratorKickoffPrompt is the first user message delivered to the
// orchestrator brain so it actually takes a turn. The conversational runner
// only sends an initial message when RunConfig.Prompt is non-empty; without it
// the agent is parked in StatePaused, idle on stdin, and never bootstraps its
// monitor loop or dispatches work (it just sits silent forever).
const orchestratorKickoffPrompt = "Start your orchestrator session now. Follow your " +
	"operating instructions in CLAUDE.md: check current board health with " +
	"`sybra-cli --json board`, then triage and dispatch ready work."

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

func selectOrchestratorSingleton(currentID string, agents []*agent.Agent) (keepID string, stopIDs []string) {
	var keep *agent.Agent
	stopSeen := map[string]struct{}{}
	addStop := func(id string) {
		if id == "" {
			return
		}
		if _, seen := stopSeen[id]; seen {
			return
		}
		stopSeen[id] = struct{}{}
		stopIDs = append(stopIDs, id)
	}

	for _, a := range agents {
		if a == nil || a.ID == "" {
			continue
		}
		if a.Name != orchestratorAgentName {
			if a.ID == currentID && orchestratorReplaceable(a) {
				addStop(a.ID)
			}
			continue
		}
		if orchestratorReplaceable(a) {
			addStop(a.ID)
			continue
		}
		if keep == nil {
			keep = a
			continue
		}

		switch {
		case keep.ID == currentID:
			addStop(a.ID)
		case a.ID == currentID:
			addStop(keep.ID)
			keep = a
		case a.StartedAt.After(keep.StartedAt):
			addStop(keep.ID)
			keep = a
		default:
			addStop(a.ID)
		}
	}

	if keep == nil {
		return "", stopIDs
	}
	return keep.ID, stopIDs
}

// OrchestratorService exposes orchestrator session operations as Wails-bound methods.
type OrchestratorService struct {
	agents    *agent.Manager
	audit     *audit.Logger
	logger    *slog.Logger
	emit      func(string, any)
	abTesting abtest.Config

	mu      sync.Mutex
	agentID string
}

func (s *OrchestratorService) reconcileOrchestratorsLocked() string {
	if s.agents == nil {
		s.agentID = ""
		return ""
	}
	keepID, stopIDs := selectOrchestratorSingleton(s.agentID, s.agents.ListAgents())
	s.agentID = keepID
	for _, id := range stopIDs {
		if id == keepID {
			continue
		}
		if err := s.agents.StopAgent(id); err != nil && s.logger != nil {
			s.logger.Warn("orchestrator.reconcile.stop", "agent_id", id, "err", err)
		}
	}
	if len(stopIDs) > 0 && s.logger != nil {
		s.logger.Info("orchestrator.reconciled", "agent_id", keepID, "stopped", len(stopIDs))
	}
	return keepID
}

// StartOrchestrator launches the orchestrator as an in-app conversational
// agent rooted at ~/.sybra (where the brain CLAUDE.md + skills live). The
// detector/dispatch loop runs in-process in the Go backend (see
// LifecycleManager.startMonitorService); this session handles the
// judgment-driven work on top of it. Provider is resolved through the A/B suite
// (ApplyABVariant) like any other role, and health/limit failover stays on so
// the brain is not stranded when its provider is unavailable.
func (s *OrchestratorService) StartOrchestrator() error {
	return s.StartOrchestratorContext(context.Background())
}

func (s *OrchestratorService) StartOrchestratorContext(ctx context.Context) error {
	// Hold the lock across agents.Run so two concurrent callers (the 1-minute
	// auto-start loop and the UI Start button) cannot both pass the replaceable
	// guard and leak an orphan brain that takes a real turn. Start is rare and
	// off any hot path, so holding the lock over the bounded process spawn is
	// acceptable.
	s.mu.Lock()
	defer s.mu.Unlock()

	if id := s.reconcileOrchestratorsLocked(); id != "" {
		return conflictError("orchestrator already running")
	}

	a, err := s.agents.RunContext(ctx, s.agents.ApplyABVariant(agent.RunConfig{
		Name:                   orchestratorAgentName,
		Mode:                   "interactive",
		Dir:                    config.HomeDir(),
		Prompt:                 orchestratorKickoffPrompt,
		IgnoreConcurrencyLimit: true,
	}, s.abTesting, "", orchestratorRole))
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
	id := s.reconcileOrchestratorsLocked()
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
	id := s.reconcileOrchestratorsLocked()
	s.mu.Unlock()
	return id != ""
}

// GetOrchestratorAgentID returns the current orchestrator agent id, or empty
// if none is running. The frontend uses this to subscribe to agent:convo:<id>
// events for live streaming.
func (s *OrchestratorService) GetOrchestratorAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileOrchestratorsLocked()
}
