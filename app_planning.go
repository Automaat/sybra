package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/notification"
	"github.com/Automaat/synapse/internal/task"
)

// TaskWorkflow handles triage, planning, evaluation, and agent completion callbacks.
type TaskWorkflow struct {
	tasks     *task.Store
	agents    *agent.Manager
	audit     *audit.Logger
	logger    *slog.Logger
	notifier  *notification.Emitter
	agentOrch *AgentOrchestrator
}

func newTaskWorkflow(
	tasks *task.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	notifier *notification.Emitter,
	agentOrch *AgentOrchestrator,
) *TaskWorkflow {
	return &TaskWorkflow{
		tasks:     tasks,
		agents:    agents,
		audit:     al,
		logger:    logger,
		notifier:  notifier,
		agentOrch: agentOrch,
	}
}

func (w *TaskWorkflow) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if w.audit == nil {
		return
	}
	if err := w.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		w.logger.Error("audit.log", "type", eventType, "err", err)
	}
}

func (w *TaskWorkflow) TriageTask(id string) error {
	t, err := w.tasks.Get(id)
	if err != nil {
		return err
	}

	dir := config.HomeDir()
	prompt := fmt.Sprintf("Triage task %s using /synapse-triage skill. Get the task with synapse-cli, analyze it, assign tags, mode, and update status.", t.ID)

	w.logger.Info("triage.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := w.agents.Run(agent.RunConfig{
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
	if err := w.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "triage", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		w.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	w.logger.Info("triage.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (w *TaskWorkflow) PlanTask(id string) error {
	t, err := w.tasks.Get(id)
	if err != nil {
		return err
	}

	t = w.agentOrch.autoAssignProject(t)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := w.agentOrch.prepareWorktree(t)
		if wtErr != nil {
			return fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	prompt := fmt.Sprintf(
		"Plan task %s using /synapse-plan skill. Get the task with synapse-cli, "+
			"analyze the codebase, and produce a detailed implementation plan. "+
			"Do NOT implement anything.", t.ID)

	w.logger.Info("plan.start", "task_id", t.ID, "title", t.Title)

	planDir := dir
	if planDir == "" {
		planDir = config.HomeDir()
	}
	ag, err := w.agents.Run(agent.RunConfig{
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
	if err := w.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "plan", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		w.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	w.logger.Info("plan.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (w *TaskWorkflow) ApprovePlan(id string) (task.Task, error) {
	t, err := w.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Status != task.StatusPlanReview {
		return task.Task{}, fmt.Errorf("task %s status is %q, expected 'plan-review'", id, t.Status)
	}
	w.logger.Info("plan.approve", "task_id", id, "title", t.Title)
	w.logAudit(audit.EventPlanApproved, id, "", map[string]any{"title": t.Title})
	updated, err := w.tasks.Update(id, map[string]any{
		"status": string(task.StatusInProgress),
	})
	if err != nil {
		return updated, err
	}
	if !w.agents.HasRunningAgentForTask(id) && !slices.Contains(updated.Tags, "review") {
		go func() {
			if _, err := w.agentOrch.StartAgent(id, updated.AgentMode, "Implement this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
				w.logger.Error("auto-implement.failed", "task_id", id, "err", err)
			}
		}()
	}
	return updated, nil
}

func (w *TaskWorkflow) RejectPlan(id, feedback string) (task.Task, error) {
	t, err := w.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Status != task.StatusPlanReview {
		return task.Task{}, fmt.Errorf("task %s status is %q, expected 'plan-review'", id, t.Status)
	}
	w.logger.Info("plan.reject", "task_id", id, "title", t.Title, "has_feedback", feedback != "")
	w.logAudit(audit.EventPlanRejected, id, "", map[string]any{"title": t.Title, "has_feedback": feedback != ""})
	body := t.Body
	if feedback != "" {
		body += "\n\n## Plan Feedback\n\n" + feedback
	}
	updated, err := w.tasks.Update(id, map[string]any{
		"status": string(task.StatusPlanning),
		"body":   body,
	})
	if err != nil {
		return updated, err
	}
	go func() {
		if planErr := w.PlanTask(id); planErr != nil {
			w.logger.Error("plan.reject.replan", "task_id", id, "err", planErr)
		}
	}()
	return updated, nil
}

func (w *TaskWorkflow) EvaluateTask(taskID, agentResult string) error {
	t, err := w.tasks.Get(taskID)
	if err != nil {
		return err
	}

	if t.Status != task.StatusInProgress {
		w.logger.Info("eval.skip", "task_id", t.ID, "status", string(t.Status), "reason", "not_in_progress")
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
			"   - Failed, errors, incomplete → human-required (MUST include --status-reason explaining why)\n"+
			"   - Never set done or todo\n"+
			"   Run: synapse-cli --json update %s --status <status> [--status-reason \"reason\"]\n\n"+
			"## Agent Result\n\n%s",
		t.ID, t.ID, t.ID, t.ID, truncated)

	w.logger.Info("eval.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := w.agents.Run(agent.RunConfig{
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
	if err := w.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "eval", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		w.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	w.logger.Info("eval.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (w *TaskWorkflow) handleAgentComplete(ag *agent.Agent) {
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
	if err := w.tasks.UpdateRun(ag.TaskID, ag.ID, map[string]any{
		"state":    string(ag.State),
		"cost_usd": ag.CostUSD,
		"result":   truncatedResult,
	}); err != nil {
		w.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}

	// Notify for non-system agents
	if !strings.HasPrefix(ag.Name, "triage:") && !strings.HasPrefix(ag.Name, "eval:") {
		level := notification.LevelSuccess
		title := "Agent completed"
		if ag.State == agent.StateStopped && !hasResultEvent(ag) {
			level = notification.LevelError
			title = "Agent failed"
		}
		w.notifier.Send(level, title, ag.Name, ag.TaskID, ag.ID)
	}

	if strings.HasPrefix(ag.Name, "triage:") || strings.HasPrefix(ag.Name, "eval:") {
		w.logger.Info("eval.skip", "agent_id", ag.ID, "name", ag.Name, "reason", "system_agent")
		w.logAudit(audit.EventTriageCompleted, ag.TaskID, ag.ID, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "pr-fix:") {
		w.logger.Info("pr-fix.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		w.logAudit(audit.EventPRFixAgentStarted, ag.TaskID, ag.ID, agentData)
		go func() {
			if err := w.EvaluateTask(ag.TaskID, resultContent); err != nil {
				w.logger.Error("pr-fix.eval", "task_id", ag.TaskID, "err", err)
			}
		}()
		return
	}

	if strings.HasPrefix(ag.Name, "plan:") {
		w.completePlanAgent(ag, resultContent, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "eval:") {
		w.completeEvalAgent(ag, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "review:") {
		w.logger.Info("review.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		w.logAudit(audit.EventReviewStarted, ag.TaskID, ag.ID, agentData)
		if _, err := w.tasks.Update(ag.TaskID, map[string]any{"reviewed": true}); err != nil {
			w.logger.Error("review.mark-reviewed", "task_id", ag.TaskID, "err", err)
		}
		go w.resolveReviewStatus(ag.TaskID)
		return
	}

	eventType := audit.EventAgentCompleted
	if ag.State != agent.StateStopped {
		eventType = audit.EventAgentFailed
	}
	w.logAudit(eventType, ag.TaskID, ag.ID, agentData)

	go func() {
		if err := w.EvaluateTask(ag.TaskID, resultContent); err != nil {
			w.logger.Error("auto-evaluate.failed", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
		}
	}()

	// Cleanup worktree if task is done and no other agent is running.
	if t, err := w.tasks.Get(ag.TaskID); err == nil && t.Status == task.StatusDone {
		go w.agentOrch.cleanupWorktree(ag.TaskID)
	}
}

func (w *TaskWorkflow) completePlanAgent(ag *agent.Agent, resultContent string, agentData map[string]any) {
	w.logger.Info("plan.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
	w.logAudit(audit.EventPlanCompleted, ag.TaskID, ag.ID, agentData)
	body := ""
	if t, err := w.tasks.Get(ag.TaskID); err == nil {
		body = t.Body
	}
	if resultContent != "" {
		body += "\n\n## Plan\n\n" + resultContent
	}
	if _, err := w.tasks.Update(ag.TaskID, map[string]any{
		"status": string(task.StatusPlanReview),
		"body":   body,
	}); err != nil {
		w.logger.Error("plan.update", "task_id", ag.TaskID, "err", err)
	}
}

func (w *TaskWorkflow) completeEvalAgent(ag *agent.Agent, agentData map[string]any) {
	w.logger.Info("eval.done", "agent_id", ag.ID, "name", ag.Name)
	w.logAudit(audit.EventEvalCompleted, ag.TaskID, ag.ID, agentData)
	t, err := w.tasks.Get(ag.TaskID)
	if err != nil {
		return
	}
	if t.Status == task.StatusDone {
		w.logger.Warn("eval.reverted_done", "agent_id", ag.ID, "task_id", ag.TaskID)
		if _, uerr := w.tasks.Update(ag.TaskID, map[string]any{
			"status": string(task.StatusInReview),
		}); uerr != nil {
			w.logger.Error("eval.revert_status", "task_id", ag.TaskID, "err", uerr)
		}
	}
}

func (w *TaskWorkflow) resolveReviewStatus(taskID string) {
	t, err := w.tasks.Get(taskID)
	if err != nil {
		return
	}
	if t.PRNumber == 0 || t.ProjectID == "" {
		if _, err := w.tasks.Update(taskID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			w.logger.Error("review.status-update", "task_id", taskID, "err", err)
		}
		return
	}

	pending, err := github.HasPendingReview(t.ProjectID, t.PRNumber)
	if err != nil {
		w.logger.Warn("review.pending-check", "task_id", taskID, "err", err)
		if _, err := w.tasks.Update(taskID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			w.logger.Error("review.status-update", "task_id", taskID, "err", err)
		}
		return
	}

	nextStatus := task.StatusInReview
	if pending {
		nextStatus = task.StatusHumanRequired
	}
	if _, err := w.tasks.Update(taskID, map[string]any{"status": string(nextStatus)}); err != nil {
		w.logger.Error("review.status-update", "task_id", taskID, "err", err)
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
