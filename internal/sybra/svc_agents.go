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
//
// Use this for headless-mode agents. Interactive agents persist a different
// log format (raw Claude stream-json envelope); route those through
// GetAgentRunConvoLog so the nested message content is preserved.
func (s *AgentService) GetAgentRunLog(taskID, agentID string) ([]agent.StreamEvent, error) {
	if ag, err := s.agents.GetAgent(agentID); err == nil {
		return ag.Output(), nil
	}

	logFile, err := s.findAgentLogFile(taskID, agentID)
	if err != nil {
		return nil, err
	}
	return agent.ParseLogFile(logFile, s.cfg.DefaultMaxLogEvents())
}

// GetAgentRunConvoLog returns conversation events for a past or running
// interactive agent. Live agents return the in-memory convo buffer;
// completed agents are replayed from the NDJSON log via the Claude stream
// parser so tool_use/tool_result/text blocks survive.
//
// This exists because interactive logs are written in the raw Anthropic
// envelope format; ParseLogFile (flat StreamEvent) silently drops the
// nested `message.content[]` and the frontend renders empty bubbles.
func (s *AgentService) GetAgentRunConvoLog(taskID, agentID string) ([]agent.ConvoEvent, error) {
	if s.agents != nil {
		if events, err := s.agents.GetConvoOutput(agentID); err == nil && len(events) > 0 {
			return events, nil
		}
	}

	logFile, err := s.findAgentLogFile(taskID, agentID)
	if err != nil {
		s.logger.Warn("agent.convo-log.not-found",
			"task_id", taskID, "agent_id", agentID, "err", err)
		return nil, err
	}

	s.logger.Info("agent.convo-log.replay",
		"task_id", taskID, "agent_id", agentID, "log", logFile)
	events, parseErr := agent.ParseConvoLogFile(logFile, s.cfg.DefaultMaxLogEvents(), s.logger)
	if parseErr != nil {
		s.logger.Error("agent.convo-log.parse-failed",
			"task_id", taskID, "agent_id", agentID, "log", logFile, "err", parseErr)
		return nil, parseErr
	}
	return events, nil
}

// findAgentLogFile resolves the NDJSON log path for an agent run, first
// consulting the task's agent_runs history then falling back to filesystem
// globbing by agent ID. Shared by GetAgentRunLog / GetAgentRunConvoLog.
func (s *AgentService) findAgentLogFile(taskID, agentID string) (string, error) {
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return "", fmt.Errorf("task %s: %w", taskID, err)
	}

	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID == agentID && t.AgentRuns[i].LogFile != "" {
			return t.AgentRuns[i].LogFile, nil
		}
	}

	return agent.FindLogFile(s.logsDir, agentID)
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
