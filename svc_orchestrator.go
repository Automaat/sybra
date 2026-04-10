package main

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
)

// orchestratorAgentName is the stable Name assigned to the orchestrator agent
// so the frontend and tests can identify it in agent listings.
const orchestratorAgentName = "orchestrator"

// orchestratorInitialPrompt bootstraps the periodic monitor loop. See
// .claude/skills/synapse-monitor.md and the loop skill.
const orchestratorInitialPrompt = "/loop 5m /synapse-monitor"

// OrchestratorService exposes orchestrator session operations as Wails-bound methods.
type OrchestratorService struct {
	agents *agent.Manager
	audit  *audit.Logger
	logger *slog.Logger
	emit   func(string, any)
	cfg    *config.Config

	mu      sync.Mutex
	agentID string
}

// StartOrchestrator launches the orchestrator as an in-app conversational
// Claude agent rooted at ~/.synapse (where the brain CLAUDE.md + skills live)
// and seeds it with /loop 5m /synapse-monitor so the periodic monitor cycle
// begins immediately. Provider is pinned to claude because /loop and
// /synapse-monitor are Claude-only skills.
func (s *OrchestratorService) StartOrchestrator() error {
	s.mu.Lock()
	if id := s.agentID; id != "" {
		if a, err := s.agents.GetAgent(id); err == nil && a.GetState() != agent.StateStopped {
			s.mu.Unlock()
			return fmt.Errorf("orchestrator already running")
		}
		s.agentID = ""
	}
	s.mu.Unlock()

	a, err := s.agents.Run(agent.RunConfig{
		Name:                   orchestratorAgentName,
		Mode:                   "interactive",
		Prompt:                 orchestratorInitialPrompt,
		Dir:                    config.HomeDir(),
		Provider:               "claude",
		IgnoreConcurrencyLimit: true,
	})
	if err != nil {
		return fmt.Errorf("start orchestrator agent: %w", err)
	}

	s.mu.Lock()
	s.agentID = a.ID
	s.mu.Unlock()

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
		return fmt.Errorf("orchestrator not running")
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
	return a.GetState() != agent.StateStopped
}

// GetOrchestratorAgentID returns the current orchestrator agent id, or empty
// if none is running. The frontend uses this to subscribe to agent:convo:<id>
// events for live streaming.
func (s *OrchestratorService) GetOrchestratorAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentID
}
