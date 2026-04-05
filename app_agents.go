package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
)

// AgentOrchestrator manages agent lifecycle: worktree setup, project
// assignment, and agent launching for a task.
type AgentOrchestrator struct {
	tasks        *task.Store
	projects     *project.Store
	agents       *agent.Manager
	audit        *audit.Logger
	logger       *slog.Logger
	worktreesDir string
}

func newAgentOrchestrator(
	tasks *task.Store,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	worktreesDir string,
) *AgentOrchestrator {
	return &AgentOrchestrator{
		tasks:        tasks,
		projects:     projects,
		agents:       agents,
		audit:        al,
		logger:       logger,
		worktreesDir: worktreesDir,
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
	if t.Status != task.StatusInProgress {
		if _, err := o.tasks.Update(taskID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			o.logger.Error("task.auto-status", "task_id", taskID, "err", err)
		}
	}

	t = o.autoAssignProject(t)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := o.prepareWorktree(t)
		if wtErr != nil {
			return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := o.agents.Run(agent.RunConfig{
		TaskID:       taskID,
		Name:         t.Title,
		Mode:         mode,
		Prompt:       fullPrompt,
		AllowedTools: t.AllowedTools,
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return nil, err
	}
	o.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": mode, "title": t.Title})
	if err := o.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Mode:      mode,
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

func (o *AgentOrchestrator) prepareWorktree(t task.Task) (string, error) {
	proj, err := o.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		o.logger.Warn("worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := project.DefaultBranch(proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	wtPath := filepath.Join(o.worktreesDir, t.DirName())
	wtBranch := "synapse/" + t.DirName()
	if _, statErr := os.Stat(wtPath); statErr == nil {
		if err := project.SanitizeWorktree(wtPath); err != nil {
			o.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
		}
		if t.Branch == "" {
			if _, err := o.tasks.Update(t.ID, map[string]any{"branch": wtBranch}); err != nil {
				o.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
			}
		}
		return wtPath, nil
	}
	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, "origin/"+branch); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}

	o.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)

	if err := project.PushUpstream(wtPath, wtBranch); err != nil {
		o.logger.Warn("worktree.push-upstream", "task_id", t.ID, "branch", wtBranch, "err", err)
	}

	if t.Branch == "" {
		if _, err := o.tasks.Update(t.ID, map[string]any{"branch": wtBranch}); err != nil {
			o.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
		}
	}

	return wtPath, nil
}

func (o *AgentOrchestrator) cleanupWorktree(taskID string) {
	t, err := o.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return
	}
	wtPath := filepath.Join(o.worktreesDir, t.DirName())
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	proj, err := o.projects.Get(t.ProjectID)
	if err != nil {
		return
	}

	if err := project.RemoveWorktree(proj.ClonePath, wtPath); err != nil {
		o.logger.Error("worktree.cleanup", "path", wtPath, "err", err)
	} else {
		o.logger.Info("worktree.cleaned", "path", wtPath)
	}
}

// cleanupOrphanedWorktrees scans the worktrees directory and removes entries
// that no longer have an active task or running agent.
func (a *App) cleanupOrphanedWorktrees() {
	entries, err := os.ReadDir(a.worktreesDir)
	if err != nil {
		return
	}
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	// Build lookup: dirName → task
	active := make(map[string]*task.Task, len(tasks))
	for i := range tasks {
		active[tasks[i].DirName()] = &tasks[i]
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		wtPath := filepath.Join(a.worktreesDir, name)

		t, exists := active[name]
		switch {
		case !exists:
			// Task deleted — remove worktree directory.
		case t.Status != task.StatusDone:
			continue
		case a.agents.HasRunningAgentForTask(t.ID):
			continue
		}

		if err := os.RemoveAll(wtPath); err != nil {
			a.logger.Error("worktree.orphan-cleanup", "path", wtPath, "err", err)
		} else {
			a.logger.Info("worktree.orphan-cleaned", "path", wtPath)
		}
	}
}

// startPRFixReviewAgent starts a headless agent to address review comments on
// the task's PR. Named "pr-fix:" so handleAgentComplete routes it correctly.
func (a *App) startPRFixReviewAgent(taskID string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}

	t = a.agentOrch.autoAssignProject(t)
	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.agentOrch.prepareWorktree(t)
		if wtErr != nil {
			return fmt.Errorf("worktree required: %w", wtErr)
		}
		dir = d
	}

	prompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\nFix the issues raised in the PR review. Push the changes when done.", t.Title, t.Body)
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       taskID,
		Name:         agent.RolePRFix.AgentName(t.Title),
		Mode:         t.AgentMode,
		Prompt:       prompt,
		AllowedTools: t.AllowedTools,
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return err
	}

	a.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": t.AgentMode, "title": t.Title, "role": "pr-fix"})
	if err := a.tasks.AddRun(taskID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RolePRFix), Mode: t.AgentMode,
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
