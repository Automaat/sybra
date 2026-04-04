package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/notification"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
)

func (a *App) StartAgent(taskID, mode, prompt string) (*agent.Agent, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status != task.StatusInProgress {
		if _, err := a.tasks.Update(taskID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			a.logger.Error("task.auto-status", "task_id", taskID, "err", err)
		}
	}

	t = a.autoAssignProject(t)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := a.agents.Run(agent.RunConfig{
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
	a.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": mode, "title": t.Title})
	if err := a.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Mode:      mode,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return ag, nil
}

func (a *App) autoAssignProject(t task.Task) task.Task {
	if t.ProjectID != "" || a.projects == nil {
		return t
	}
	projects, err := a.projects.List()
	if err != nil || len(projects) != 1 {
		return t
	}
	t.ProjectID = projects[0].ID
	if _, err := a.tasks.Update(t.ID, map[string]any{"project_id": t.ProjectID}); err != nil {
		a.logger.Error("auto-assign-project", "task_id", t.ID, "err", err)
	} else {
		a.logger.Info("auto-assign-project", "task_id", t.ID, "project", t.ProjectID)
	}
	return t
}

func (a *App) prepareWorktree(t task.Task) (string, error) {
	proj, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		a.logger.Warn("worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := project.DefaultBranch(proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	wtPath := filepath.Join(a.worktreesDir, t.DirName())
	wtBranch := "synapse/" + t.DirName()
	if _, statErr := os.Stat(wtPath); statErr == nil {
		if t.Branch == "" {
			if _, err := a.tasks.Update(t.ID, map[string]any{"branch": wtBranch}); err != nil {
				a.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
			}
		}
		return wtPath, nil
	}
	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, "refs/remotes/origin/"+branch); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}

	a.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)

	if t.Branch == "" {
		if _, err := a.tasks.Update(t.ID, map[string]any{"branch": wtBranch}); err != nil {
			a.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
		}
	}

	return wtPath, nil
}

func (a *App) cleanupWorktree(taskID string) {
	if a.agents.HasRunningAgentForTask(taskID) {
		a.logger.Info("worktree.cleanup.deferred", "task_id", taskID, "reason", "agent_running")
		return
	}
	t, err := a.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return
	}
	wtPath := filepath.Join(a.worktreesDir, t.DirName())
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	proj, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return
	}

	if err := project.RemoveWorktree(proj.ClonePath, wtPath); err != nil {
		a.logger.Error("worktree.cleanup", "path", wtPath, "err", err)
	} else {
		a.logger.Info("worktree.cleaned", "path", wtPath)
	}
}

func (a *App) StopAgent(agentID string) error {
	return a.agents.StopAgent(agentID)
}

func (a *App) ListAgents() []*agent.Agent {
	return a.agents.ListAgents()
}

func (a *App) DiscoverAgents() []*agent.Agent {
	return a.agents.DiscoverAgents()
}

func (a *App) CaptureAgentPane(agentID string) (string, error) {
	return a.agents.CapturePane(agentID)
}

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

func (a *App) ListTmuxSessions() ([]tmux.SessionInfo, error) {
	return a.tmux.ListSessions()
}

func (a *App) KillTmuxSession(name string) error {
	a.logger.Info("tmux.kill", "session", name)
	return a.tmux.KillSession(name)
}

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

func (a *App) GetAgentOutput(agentID string) ([]agent.StreamEvent, error) {
	ag, err := a.agents.GetAgent(agentID)
	if err != nil {
		return nil, err
	}
	return ag.Output(), nil
}

func (a *App) handleAgentComplete(ag *agent.Agent) {
	var resultContent string
	for _, ev := range ag.Output() {
		if ev.Type == "result" {
			resultContent = ev.Content
		}
	}

	duration := time.Since(ag.StartedAt).Seconds()
	agentData := map[string]any{
		"mode":       ag.Mode,
		"cost_usd":   ag.CostUSD,
		"duration_s": duration,
		"state":      string(ag.State),
	}

	// Persist run result to task file
	truncatedResult := resultContent
	if len(truncatedResult) > 2000 {
		truncatedResult = truncatedResult[:2000] + "\n... (truncated)"
	}
	if err := a.tasks.UpdateRun(ag.TaskID, ag.ID, map[string]any{
		"state":    string(ag.State),
		"cost_usd": ag.CostUSD,
		"result":   truncatedResult,
	}); err != nil {
		a.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}

	// Notify for non-system agents
	if !strings.HasPrefix(ag.Name, "triage:") && !strings.HasPrefix(ag.Name, "eval:") {
		level := notification.LevelSuccess
		title := "Agent completed"
		if ag.State == agent.StateStopped && !hasResultEvent(ag) {
			level = notification.LevelError
			title = "Agent failed"
		}
		a.notifier.Send(level, title, ag.Name, ag.TaskID, ag.ID)
	}

	if strings.HasPrefix(ag.Name, "triage:") || strings.HasPrefix(ag.Name, "eval:") {
		a.logger.Info("eval.skip", "agent_id", ag.ID, "name", ag.Name, "reason", "system_agent")
		a.logAudit(audit.EventTriageCompleted, ag.TaskID, ag.ID, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "pr-fix:") {
		a.logger.Info("pr-fix.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		a.logAudit(audit.EventPRFixAgentStarted, ag.TaskID, ag.ID, agentData)
		go func() {
			if err := a.EvaluateTask(ag.TaskID, resultContent); err != nil {
				a.logger.Error("pr-fix.eval", "task_id", ag.TaskID, "err", err)
			}
		}()
		return
	}

	if strings.HasPrefix(ag.Name, "plan:") {
		a.completePlanAgent(ag, resultContent, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "eval:") {
		a.completeEvalAgent(ag, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "review:") {
		a.logger.Info("review.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		a.logAudit(audit.EventReviewStarted, ag.TaskID, ag.ID, agentData)
		if _, err := a.tasks.Update(ag.TaskID, map[string]any{"reviewed": true}); err != nil {
			a.logger.Error("review.mark-reviewed", "task_id", ag.TaskID, "err", err)
		}
		go a.resolveReviewStatus(ag.TaskID)
		return
	}

	eventType := audit.EventAgentCompleted
	if ag.State != agent.StateStopped {
		eventType = audit.EventAgentFailed
	}
	a.logAudit(eventType, ag.TaskID, ag.ID, agentData)

	// Cleanup worktree if task was already marked done while agent was running.
	if t, err := a.tasks.Get(ag.TaskID); err == nil && t.Status == task.StatusDone {
		go a.cleanupWorktree(ag.TaskID)
		return
	}

	a.wg.Go(func() {
		if err := a.EvaluateTask(ag.TaskID, resultContent); err != nil {
			a.logger.Error("auto-evaluate.failed", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
		}
	})
}

func (a *App) completePlanAgent(ag *agent.Agent, resultContent string, agentData map[string]any) {
	a.logger.Info("plan.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
	a.logAudit(audit.EventPlanCompleted, ag.TaskID, ag.ID, agentData)
	body := ""
	if t, err := a.tasks.Get(ag.TaskID); err == nil {
		body = t.Body
	}
	if resultContent != "" {
		body += "\n\n## Plan\n\n" + resultContent
	}
	if _, err := a.tasks.Update(ag.TaskID, map[string]any{
		"status": string(task.StatusPlanReview),
		"body":   body,
	}); err != nil {
		a.logger.Error("plan.update", "task_id", ag.TaskID, "err", err)
	}
}

func (a *App) completeEvalAgent(ag *agent.Agent, agentData map[string]any) {
	a.logger.Info("eval.done", "agent_id", ag.ID, "name", ag.Name)
	a.logAudit(audit.EventEvalCompleted, ag.TaskID, ag.ID, agentData)
	t, err := a.tasks.Get(ag.TaskID)
	if err != nil {
		return
	}
	if t.Status == task.StatusDone {
		a.logger.Warn("eval.reverted_done", "agent_id", ag.ID, "task_id", ag.TaskID)
		if _, uerr := a.tasks.Update(ag.TaskID, map[string]any{
			"status": string(task.StatusInReview),
		}); uerr != nil {
			a.logger.Error("eval.revert_status", "task_id", ag.TaskID, "err", uerr)
		}
	}
}

func hasResultEvent(ag *agent.Agent) bool {
	for _, ev := range ag.Output() {
		if ev.Type == "result" {
			return true
		}
	}
	return false
}
