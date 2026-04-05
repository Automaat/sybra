package main

import (
	"slices"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/task"
)

// ListTasks returns all tasks from the task store.
func (a *App) ListTasks() ([]task.Task, error) {
	return a.tasks.List()
}

// GetTask returns a single task by ID.
func (a *App) GetTask(id string) (task.Task, error) {
	return a.tasks.Get(id)
}

// CreateTask creates a new task and triggers auto-triage for todo tasks.
func (a *App) CreateTask(title, body, mode string) (task.Task, error) {
	t, err := a.tasks.Create(title, body, mode)
	if err != nil {
		return t, err
	}
	a.logAudit(audit.EventTaskCreated, t.ID, "", map[string]any{"title": title, "mode": mode})
	if t.Status == task.StatusTodo {
		a.logger.Info("auto-triage.start", "task_id", t.ID, "title", t.Title)
		a.wg.Go(func() {
			if triageErr := a.TriageTask(t.ID); triageErr != nil {
				a.logger.Error("auto-triage.failed", "task_id", t.ID, "err", triageErr)
			}
		})
	}
	return t, nil
}

// UpdateTask applies field updates to a task and triggers auto-planning or
// auto-implementation based on the resulting status.
func (a *App) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	var prevStatus string
	if newStatus, ok := updates["status"].(string); ok {
		if prev, getErr := a.tasks.Get(id); getErr == nil {
			prevStatus = string(prev.Status)
			if prevStatus != newStatus {
				a.logAudit(audit.EventTaskStatusChanged, id, "", map[string]any{"from": prevStatus, "to": newStatus})
			}
		}
	}
	t, err := a.tasks.Update(id, updates)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusPlanning {
		a.logger.Info("auto-plan.start", "task_id", t.ID, "title", t.Title)
		a.wg.Go(func() {
			if planErr := a.PlanTask(t.ID); planErr != nil {
				a.logger.Error("auto-plan.failed", "task_id", t.ID, "err", planErr)
			}
		})
	}
	if t.Status == task.StatusInProgress && !a.agents.HasRunningAgentForTask(t.ID) && !slices.Contains(t.Tags, "review") {
		if prevStatus == string(task.StatusInReview) {
			a.logger.Info("auto-fix-review.start", "task_id", t.ID, "title", t.Title)
			if _, err := a.tasks.Update(t.ID, map[string]any{"run_role": "pr-fix"}); err != nil {
				a.logger.Error("auto-fix-review.set-role", "task_id", t.ID, "err", err)
			}
			a.wg.Go(func() {
				if err := a.startPRFixReviewAgent(t.ID); err != nil {
					a.logger.Error("auto-fix-review.failed", "task_id", t.ID, "err", err)
				}
			})
		} else {
			a.logger.Info("auto-implement.start", "task_id", t.ID, "title", t.Title)
			a.wg.Go(func() {
				if _, err := a.agentOrch.StartAgent(t.ID, t.AgentMode, "Implement this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
					a.logger.Error("auto-implement.failed", "task_id", t.ID, "err", err)
				}
			})
		}
	}
	if t.Status == task.StatusDone {
		a.wg.Go(func() { a.agentOrch.cleanupWorktree(t.ID) })
	}
	return t, nil
}

// DeleteTask removes a task file from disk and cleans up its worktree.
func (a *App) DeleteTask(id string) error {
	a.logger.Info("task.delete", "task_id", id)
	a.agentOrch.cleanupWorktree(id)
	a.logAudit(audit.EventTaskDeleted, id, "", nil)
	if err := a.tasks.Delete(id); err != nil {
		a.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return err
	}
	return nil
}
