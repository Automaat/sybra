package main

import (
	"fmt"
	"log/slog"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/tmux"
)

// AgentService exposes agent and tmux session operations as Wails-bound methods.
type AgentService struct {
	agents   *agent.Manager
	tmux     *tmux.Manager
	logger   *slog.Logger
	approval *agent.ApprovalServer
}

// StopAgent sends a stop signal to the given agent.
func (s *AgentService) StopAgent(agentID string) error {
	return s.agents.StopAgent(agentID)
}

// ListAgents returns all in-memory agents (managed and external).
func (s *AgentService) ListAgents() []*agent.Agent {
	return s.agents.ListAgents()
}

// DiscoverAgents scans running Claude processes and refreshes state.
func (s *AgentService) DiscoverAgents() []*agent.Agent {
	return s.agents.DiscoverAgents()
}

// GetAgentOutput returns buffered stream events for a headless agent.
func (s *AgentService) GetAgentOutput(agentID string) ([]agent.StreamEvent, error) {
	ag, err := s.agents.GetAgent(agentID)
	if err != nil {
		return nil, err
	}
	return ag.Output(), nil
}

// CaptureAgentPane captures the current tmux pane output for an interactive agent.
func (s *AgentService) CaptureAgentPane(agentID string) (string, error) {
	return s.agents.CapturePane(agentID)
}

// AttachAgent opens the tmux session for an interactive agent in Ghostty.
func (s *AgentService) AttachAgent(agentID string) error {
	ag, err := s.agents.GetAgent(agentID)
	if err != nil {
		return err
	}
	if ag.TmuxSession == "" {
		return fmt.Errorf("agent %s has no tmux session", agentID)
	}
	title := ag.Name
	if title == "" {
		title = ag.TaskID
	}
	return openTmuxInGhostty(ag.TmuxSession, title)
}

// ListTmuxSessions returns all active tmux sessions.
func (s *AgentService) ListTmuxSessions() ([]tmux.SessionInfo, error) {
	return s.tmux.ListSessions()
}

// KillTmuxSession terminates the named tmux session.
func (s *AgentService) KillTmuxSession(name string) error {
	s.logger.Info("tmux.kill", "session", name)
	return s.tmux.KillSession(name)
}

// AttachTmuxSession opens the named tmux session in Ghostty.
func (s *AgentService) AttachTmuxSession(name string) error {
	return openTmuxInGhostty(name, name)
}

// SendMessage sends a follow-up message to a conversational agent.
func (s *AgentService) SendMessage(agentID, text string) error {
	return s.agents.SendMessage(agentID, text)
}

// RespondApproval sends a tool approval decision from the frontend.
func (s *AgentService) RespondApproval(toolUseID string, approved bool) error {
	if s.approval == nil {
		return fmt.Errorf("approval server not initialized")
	}
	return s.approval.RespondApproval(toolUseID, approved)
}

// GetConvoOutput returns the full conversation event buffer for an agent.
func (s *AgentService) GetConvoOutput(agentID string) ([]agent.ConvoEvent, error) {
	return s.agents.GetConvoOutput(agentID)
}

// RespondEscalation sends a human decision to a guardrail-paused agent.
// continueRun=true lets the agent keep running; false kills it.
func (s *AgentService) RespondEscalation(agentID string, continueRun bool) error {
	return s.agents.RespondEscalation(agentID, continueRun)
}
