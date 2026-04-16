package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

// GetAgentDiff returns a unified diff of uncommitted changes in the task
// worktree. Returns an empty string when the worktree does not exist or
// there are no changes. Untracked files are appended as synthetic new-file
// diff blocks so the Editor tab can show them alongside modified files.
func (s *AgentService) GetAgentDiff(taskID string) (string, error) {
	if s.worktrees == nil {
		return "", nil
	}
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return "", fmt.Errorf("task %s: %w", taskID, err)
	}
	if !s.worktrees.Exists(t) {
		return "", nil
	}
	dir := s.worktrees.PathFor(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")

	diffCmd := exec.CommandContext(ctx, "git", "diff", "HEAD", "--patch", "-M", "--no-color")
	diffCmd.Dir = dir
	diffCmd.Env = env
	diffOut, err := diffCmd.Output()
	if err != nil {
		s.logger.Warn("agent.diff.git-diff-failed", "task_id", taskID, "err", err)
		return "", nil
	}

	lsCmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
	lsCmd.Dir = dir
	lsCmd.Env = env
	lsOut, err := lsCmd.Output()
	if err != nil {
		s.logger.Warn("agent.diff.ls-files-failed", "task_id", taskID, "err", err)
		return string(diffOut), nil
	}

	var sb strings.Builder
	sb.WriteString(string(diffOut))
	for relPath := range strings.SplitSeq(strings.TrimSpace(string(lsOut)), "\n") {
		if relPath == "" {
			continue
		}
		absPath := filepath.Join(dir, relPath)
		data, readErr := os.ReadFile(absPath)
		if readErr != nil {
			fmt.Fprintf(&sb, "\n--- /dev/null\n+++ b/%s\n", relPath)
			continue
		}
		const maxBytes = 100 * 1024
		if len(data) > maxBytes {
			data = data[:maxBytes]
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		fmt.Fprintf(&sb, "\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", relPath, len(lines))
		for _, line := range lines {
			sb.WriteString("+" + line + "\n")
		}
	}
	return sb.String(), nil
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
