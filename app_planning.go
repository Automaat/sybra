package main

import (
	"fmt"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/task"
)

func (a *App) TriageTask(id string) error {
	t, err := a.tasks.Get(id)
	if err != nil {
		return err
	}

	dir := config.HomeDir()
	prompt := fmt.Sprintf("Triage task %s using /synapse-triage skill. Get the task with synapse-cli, analyze it, assign tags, mode, and update status.", t.ID)

	a.logger.Info("triage.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       t.ID,
		Name:         "triage:" + t.Title,
		Mode:         "headless",
		Prompt:       prompt,
		AllowedTools: []string{"Bash", "Read", "Skill"},
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "triage", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("triage.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) EvaluateTask(taskID, agentResult string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}

	if t.Status != task.StatusInProgress {
		a.logger.Info("eval.skip", "task_id", t.ID, "status", string(t.Status), "reason", "not_in_progress")
		return nil
	}

	dir := config.HomeDir()

	truncated := agentResult
	if len(truncated) > 4000 {
		truncated = truncated[:4000] + "\n... (truncated)"
	}

	prompt := fmt.Sprintf(
		"Evaluate task %s. Do NOT read source code or review diffs.\n\n"+
			"1. Run: synapse-cli --json get %s\n"+
			"2. Check agent result below for PR URLs (github.com/.../pull/N). "+
			"If found, link: synapse-cli --json update %s --pr <number>\n"+
			"3. Set status based on agent result:\n"+
			"   - Work done, PR created/pushed → in-review\n"+
			"   - Failed, errors, incomplete → human-required\n"+
			"   - Never set done or todo\n"+
			"   Run: synapse-cli --json update %s --status <status>\n\n"+
			"## Agent Result\n\n%s",
		t.ID, t.ID, t.ID, t.ID, truncated)

	a.logger.Info("eval.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       t.ID,
		Name:         "eval:" + t.Title,
		Mode:         "headless",
		Prompt:       prompt,
		AllowedTools: []string{"Bash"},
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "eval", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("eval.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) PlanTask(id string) error {
	t, err := a.tasks.Get(id)
	if err != nil {
		return err
	}

	t = a.autoAssignProject(t)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			return fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	prompt := fmt.Sprintf(
		"Plan task %s using /synapse-plan skill. Get the task with synapse-cli, "+
			"analyze the codebase, and produce a detailed implementation plan. "+
			"Do NOT implement anything.", t.ID)

	a.logger.Info("plan.start", "task_id", t.ID, "title", t.Title)

	planDir := dir
	if planDir == "" {
		planDir = config.HomeDir()
	}
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       t.ID,
		Name:         "plan:" + t.Title,
		Mode:         "headless",
		Prompt:       prompt,
		AllowedTools: []string{"Bash", "Read", "Glob", "Grep"},
		Dir:          planDir,
		Model:        "opus",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "plan", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("plan.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) ApprovePlan(id string) (task.Task, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Status != task.StatusPlanReview {
		return task.Task{}, fmt.Errorf("task %s status is %q, expected 'plan-review'", id, t.Status)
	}
	a.logger.Info("plan.approve", "task_id", id, "title", t.Title)
	a.logAudit(audit.EventPlanApproved, id, "", map[string]any{"title": t.Title})
	return a.UpdateTask(id, map[string]any{
		"status": string(task.StatusInProgress),
	})
}

func (a *App) RejectPlan(id, feedback string) (task.Task, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Status != task.StatusPlanReview {
		return task.Task{}, fmt.Errorf("task %s status is %q, expected 'plan-review'", id, t.Status)
	}
	a.logger.Info("plan.reject", "task_id", id, "title", t.Title, "has_feedback", feedback != "")
	a.logAudit(audit.EventPlanRejected, id, "", map[string]any{"title": t.Title, "has_feedback": feedback != ""})
	body := t.Body
	if feedback != "" {
		body += "\n\n## Plan Feedback\n\n" + feedback
	}
	updated, err := a.tasks.Update(id, map[string]any{
		"status": string(task.StatusPlanning),
		"body":   body,
	})
	if err != nil {
		return updated, err
	}
	go func() {
		if planErr := a.PlanTask(id); planErr != nil {
			a.logger.Error("plan.reject.replan", "task_id", id, "err", planErr)
		}
	}()
	return updated, nil
}
