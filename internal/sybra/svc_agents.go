package sybra

import (
	"fmt"
	"log/slog"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sysopen"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// AgentService exposes agent operations as Wails-bound methods.
type AgentService struct {
	agents    *agent.Manager
	logger    *slog.Logger
	approval  *agent.ApprovalServer
	tasks     *task.Manager
	cfg       *config.Config
	logsDir   string
	worktrees *worktree.Manager
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

// GetAgentRunLog returns stream events for a past or running agent.
// Live agents return the in-memory buffer; completed agents are replayed
// from the NDJSON log file on disk.
func (s *AgentService) GetAgentRunLog(taskID, agentID string) ([]agent.StreamEvent, error) {
	if ag, err := s.agents.GetAgent(agentID); err == nil {
		return ag.Output(), nil
	}

	t, err := s.tasks.Get(taskID)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", taskID, err)
	}

	var logFile string
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID == agentID {
			logFile = t.AgentRuns[i].LogFile
			break
		}
	}

	if logFile == "" {
		var findErr error
		logFile, findErr = agent.FindLogFile(s.logsDir, agentID)
		if findErr != nil {
			return nil, findErr
		}
	}

	return agent.ParseLogFile(logFile, s.cfg.DefaultMaxLogEvents())
}

// RespondEscalation sends a human decision to a guardrail-paused agent.
// continueRun=true lets the agent keep running; false kills it.
func (s *AgentService) RespondEscalation(agentID string, continueRun bool) error {
	return s.agents.RespondEscalation(agentID, continueRun)
}

// OpenWorktree opens the git worktree for taskID in the OS file manager.
// Returns an error if the worktree does not exist or the OS command fails.
func (s *AgentService) OpenWorktree(taskID string) error {
	if s.worktrees == nil {
		return fmt.Errorf("worktree manager not available")
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return fmt.Errorf("task %s: %w", taskID, err)
	}
	if !s.worktrees.Exists(t) {
		return fmt.Errorf("no worktree for task %s", taskID)
	}
	return sysopen.Dir(s.worktrees.PathFor(t))
}
