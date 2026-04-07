package main

import (
	"log/slog"
	"slices"
	"sync"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/worktree"
)

// TaskService exposes task CRUD operations as Wails-bound methods.
type TaskService struct {
	tasks     *task.Manager
	agents    *agent.Manager
	agentOrch *AgentOrchestrator
	workflow  *TaskWorkflow
	worktrees *worktree.Manager
	reviewer  *ReviewHandler
	wg        *sync.WaitGroup
	logger    *slog.Logger
	audit     *audit.Logger
}

// ListTasks returns all tasks from the store.
func (s *TaskService) ListTasks() ([]task.Task, error) {
	return s.tasks.List()
}

// GetTask returns a single task by ID.
func (s *TaskService) GetTask(id string) (task.Task, error) {
	return s.tasks.Get(id)
}

// CreateTask creates a new task and triggers auto-triage for todo tasks.
func (s *TaskService) CreateTask(title, body, mode string) (task.Task, error) {
	t, err := s.tasks.Create(title, body, mode)
	if err != nil {
		return t, err
	}
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskCreated,
			TaskID: t.ID,
			Data:   map[string]any{"title": title, "mode": mode},
		})
	}
	if t.Status == task.StatusTodo {
		s.logger.Info("auto-triage.start", "task_id", t.ID, "title", t.Title)
		s.wg.Go(func() {
			if triageErr := s.workflow.TriageTask(t.ID); triageErr != nil {
				s.logger.Error("auto-triage.failed", "task_id", t.ID, "err", triageErr)
			}
		})
	}
	return t, nil
}

// UpdateTask applies field updates to a task and triggers auto-planning or
// auto-implementation based on the resulting status.
func (s *TaskService) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	var prevStatus string
	if _, ok := updates["status"].(string); ok {
		if prev, getErr := s.tasks.Get(id); getErr == nil {
			prevStatus = string(prev.Status)
		}
	}
	t, err := s.tasks.Update(id, updates)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusPlanning {
		s.logger.Info("auto-plan.start", "task_id", t.ID, "title", t.Title)
		s.wg.Go(func() {
			if planErr := s.workflow.PlanTask(t.ID); planErr != nil {
				s.logger.Error("auto-plan.failed", "task_id", t.ID, "err", planErr)
			}
		})
	}
	if t.Status == task.StatusInProgress && !s.agents.HasRunningAgentForTask(t.ID) && !slices.Contains(t.Tags, "review") {
		if prevStatus == string(task.StatusInReview) {
			s.logger.Info("auto-fix-review.start", "task_id", t.ID, "title", t.Title)
			if _, err := s.tasks.Update(t.ID, map[string]any{"run_role": "pr-fix"}); err != nil {
				s.logger.Error("auto-fix-review.set-role", "task_id", t.ID, "err", err)
			}
			s.wg.Go(func() {
				if err := s.agentOrch.StartPRFixAgent(t.ID); err != nil {
					s.logger.Error("auto-fix-review.failed", "task_id", t.ID, "err", err)
				}
			})
		} else {
			s.logger.Info("auto-implement.start", "task_id", t.ID, "title", t.Title)
			s.wg.Go(func() {
				if _, err := s.agentOrch.StartAgent(t.ID, t.AgentMode, "Implement this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
					s.logger.Error("auto-implement.failed", "task_id", t.ID, "err", err)
				}
			})
		}
	}
	if t.Status == task.StatusDone {
		s.wg.Go(func() { s.worktrees.Remove(t.ID) })
	}
	return t, nil
}

// DeleteTask removes a task file from disk and cleans up its worktree.
func (s *TaskService) DeleteTask(id string) error {
	s.logger.Info("task.delete", "task_id", id)
	s.worktrees.Remove(id)
	if s.audit != nil {
		_ = s.audit.Log(audit.Event{
			Type:   audit.EventTaskDeleted,
			TaskID: id,
		})
	}
	if err := s.tasks.Delete(id); err != nil {
		s.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return err
	}
	return nil
}
