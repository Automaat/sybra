package main

import (
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/worktree"
)

// ResearchMachineDir is the fixed working directory for research tasks.
const ResearchMachineDir = "/Users/marcin.skalski/sideprojects/research-machine"

// resolveExecution derives the effective mode, directory, permission mode, and
// whether project worktree setup should be skipped based on the task's type.
// hintMode is used when the task type does not force a specific mode.
func resolveExecution(t task.Task, hintMode string) (mode, dir string, requirePerm, skipWorktree bool) {
	switch t.TaskType {
	case task.TaskTypeDebug:
		return "interactive", "", true, false
	case task.TaskTypeResearch:
		return "headless", ResearchMachineDir, false, true
	default:
		return hintMode, "", false, false
	}
}

// AgentOrchestrator manages agent lifecycle: worktree setup, project
// assignment, and agent launching for a task.
type AgentOrchestrator struct {
	tasks     *task.Manager
	projects  *project.Store
	agents    *agent.Manager
	audit     *audit.Logger
	logger    *slog.Logger
	worktrees *worktree.Manager
}

func newAgentOrchestrator(
	tasks *task.Manager,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	worktrees *worktree.Manager,
) *AgentOrchestrator {
	return &AgentOrchestrator{
		tasks:     tasks,
		projects:  projects,
		agents:    agents,
		audit:     al,
		logger:    logger,
		worktrees: worktrees,
	}
}

func (o *AgentOrchestrator) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if o.audit == nil {
		return
	}
	if err := o.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		o.logger.Error("audit.log", "type", eventType, "err", err)
	}
}

func (o *AgentOrchestrator) StartAgent(taskID, mode, prompt string) (*agent.Agent, error) {
	t, err := o.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	effMode, dir, requirePerm, skipWT := resolveExecution(t, mode)
	if !skipWT {
		t = o.autoAssignProject(t)
		if t.ProjectID != "" {
			d, wtErr := o.worktrees.PrepareForTask(t)
			if wtErr != nil {
				return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
			}
			dir = d
		}
	}

	// Flip status only after worktree prep succeeds — otherwise a failed
	// prep leaves the task stuck at in-progress with no running agent,
	// which triggers restart-stale respawn loops on every app restart.
	if t.Status != task.StatusInProgress {
		if _, err := o.tasks.Update(taskID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			o.logger.Error("task.auto-status", "task_id", taskID, "err", err)
		}
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := o.agents.Run(agent.RunConfig{
		TaskID:             taskID,
		Name:               t.Title,
		Mode:               effMode,
		Prompt:             fullPrompt,
		AllowedTools:       t.AllowedTools,
		Dir:                dir,
		Model:              "sonnet",
		RequirePermissions: requirePerm,
	})
	if err != nil {
		return nil, err
	}
	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": effMode, "title": t.Title, "task_type": string(t.TaskType)})
	if err := o.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Mode:      effMode,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return ag, nil
}

func (o *AgentOrchestrator) autoAssignProject(t task.Task) task.Task {
	if t.ProjectID != "" || o.projects == nil {
		return t
	}
	projects, err := o.projects.List()
	if err != nil || len(projects) != 1 {
		return t
	}
	t.ProjectID = projects[0].ID
	if _, err := o.tasks.Update(t.ID, map[string]any{"project_id": t.ProjectID}); err != nil {
		o.logger.Error("auto-assign-project", "task_id", t.ID, "err", err)
	} else {
		o.logger.Info("auto-assign-project", "task_id", t.ID, "project", t.ProjectID)
	}
	return t
}

// startPRFixReviewAgent starts a headless agent to address review comments on
// the task's PR. Named "pr-fix:" so handleAgentComplete routes it correctly.
func (a *App) startPRFixReviewAgent(taskID string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}

	effMode, dir, requirePerm, skipWT := resolveExecution(t, t.AgentMode)
	if !skipWT {
		t = a.agentOrch.autoAssignProject(t)
		if t.ProjectID != "" {
			d, wtErr := a.worktrees.PrepareForTask(t)
			if wtErr != nil {
				return fmt.Errorf("worktree required: %w", wtErr)
			}
			dir = d
		}
	}

	prompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\nFix the issues raised in the PR review. Push the changes when done.", t.Title, t.Body)
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:             taskID,
		Name:               agent.RolePRFix.AgentName(t.Title),
		Mode:               effMode,
		Prompt:             prompt,
		AllowedTools:       t.AllowedTools,
		Dir:                dir,
		Model:              "sonnet",
		RequirePermissions: requirePerm,
	})
	if err != nil {
		return err
	}

	a.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": effMode, "title": t.Title, "role": "pr-fix", "task_type": string(t.TaskType)})
	if err := a.tasks.AddRun(taskID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RolePRFix), Mode: effMode,
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return nil
}

// StopAgent sends a stop signal to the given agent.
func (a *App) StopAgent(agentID string) error {
	return a.agents.StopAgent(agentID)
}

// ListAgents returns all in-memory agents (managed and external).
func (a *App) ListAgents() []*agent.Agent {
	return a.agents.ListAgents()
}

// DiscoverAgents scans running Claude processes, registers new external agents,
// and refreshes state of already-tracked ones.
func (a *App) DiscoverAgents() []*agent.Agent {
	return a.agents.DiscoverAgents()
}

// CaptureAgentPane captures the current tmux pane output for an interactive agent.
func (a *App) CaptureAgentPane(agentID string) (string, error) {
	return a.agents.CapturePane(agentID)
}

// AttachAgent opens the tmux session for an interactive agent in Ghostty.
func (a *App) AttachAgent(agentID string) error {
	ag, err := a.agents.GetAgent(agentID)
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
func (a *App) ListTmuxSessions() ([]tmux.SessionInfo, error) {
	return a.tmux.ListSessions()
}

// KillTmuxSession terminates the named tmux session.
func (a *App) KillTmuxSession(name string) error {
	a.logger.Info("tmux.kill", "session", name)
	return a.tmux.KillSession(name)
}

// AttachTmuxSession opens the named tmux session in Ghostty.
func (a *App) AttachTmuxSession(name string) error {
	return openTmuxInGhostty(name, name)
}

func openTmuxInGhostty(session, tabTitle string) error {
	label := "Synapse: " + tabTitle
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Synapse:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic 'printf \"\\033]0;%s\\007\"; exec tmux attach -t %s'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, label, session)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}
