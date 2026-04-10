package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/todoist"
)

// TodoistHandler syncs tasks between Todoist and Synapse.
type TodoistHandler struct {
	DomainHandler
	tasks   *task.Manager
	taskSvc *TaskService
	client  *todoist.Client
	cfg     config.TodoistConfig
}

func newTodoistHandler(
	tasks *task.Manager,
	taskSvc *TaskService,
	client *todoist.Client,
	al *audit.Logger,
	logger *slog.Logger,
	emit func(string, any),
	cfg config.TodoistConfig,
) *TodoistHandler {
	return &TodoistHandler{
		DomainHandler: DomainHandler{audit: al, logger: logger, emit: emit},
		tasks:         tasks,
		taskSvc:       taskSvc,
		client:        client,
		cfg:           cfg,
	}
}

// pollAndSync runs one import+completion cycle and returns the next poll interval.
func (h *TodoistHandler) pollAndSync() time.Duration {
	interval := time.Duration(h.cfg.PollSeconds) * time.Second

	imported, importErr := h.importNewTasks()
	if importErr != nil {
		h.logger.Error("todoist.import", "err", importErr)
	}

	completed, compErr := h.syncCompletions()
	if compErr != nil {
		h.logger.Error("todoist.complete", "err", compErr)
	}

	if imported > 0 || completed > 0 {
		h.logger.Info("todoist.synced", "imported", imported, "completed", completed)
		h.emit(events.TodoistSynced, map[string]any{
			"imported":  imported,
			"completed": completed,
		})
	}

	return interval
}

func (h *TodoistHandler) importNewTasks() (int, error) {
	remote, err := h.client.ListActiveTasks(h.cfg.ProjectID)
	if err != nil {
		return 0, err
	}

	seen, err := h.buildSeenIndex()
	if err != nil {
		return 0, fmt.Errorf("build seen index: %w", err)
	}

	var imported int
	for i := range remote {
		rt := &remote[i]
		if seen[rt.ID] {
			continue
		}
		if rt.Due != nil && rt.Due.IsRecurring {
			continue
		}

		body := rt.Description
		if rt.URL != "" {
			if body != "" {
				body += "\n\n"
			}
			body += "Source: " + rt.URL
		}

		t, createErr := h.taskSvc.CreateTask(rt.Content, body, "headless")
		if createErr != nil {
			h.logger.Error("todoist.create-task", "todoist_id", rt.ID, "err", createErr)
			continue
		}

		todoID := rt.ID
		update := task.Update{TodoistID: &todoID}
		if h.cfg.DefaultProjectID != "" {
			projectID := h.cfg.DefaultProjectID
			update.ProjectID = &projectID
		}
		if _, updateErr := h.tasks.Update(t.ID, update); updateErr != nil {
			h.logger.Error("todoist.update-task", "task_id", t.ID, "err", updateErr)
		}

		h.logAudit(audit.EventTodoistImported, t.ID, "", map[string]any{
			"todoist_id": rt.ID,
			"title":      rt.Content,
		})
		imported++
	}

	return imported, nil
}

func (h *TodoistHandler) syncCompletions() (int, error) {
	tasks, err := h.tasks.List()
	if err != nil {
		return 0, err
	}

	var completed int
	for i := range tasks {
		t := &tasks[i]
		if t.TodoistID == "" || t.Status != task.StatusDone {
			continue
		}
		if closeErr := h.client.CloseTask(t.TodoistID); closeErr != nil {
			h.logger.Error("todoist.close", "task_id", t.ID, "todoist_id", t.TodoistID, "err", closeErr)
			continue
		}
		h.logAudit(audit.EventTodoistCompleted, t.ID, "", map[string]any{
			"todoist_id": t.TodoistID,
		})
		completed++
	}

	return completed, nil
}

func (h *TodoistHandler) buildSeenIndex() (map[string]bool, error) {
	tasks, err := h.tasks.List()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		if t.TodoistID != "" {
			seen[t.TodoistID] = true
		}
	}
	return seen, nil
}

func (a *App) initTodoist(emit func(string, any)) {
	if !a.cfg.Todoist.Enabled || a.cfg.Todoist.APIToken == "" {
		return
	}
	tc := todoist.NewClient(a.cfg.Todoist.APIToken)
	a.todoistHandler = newTodoistHandler(a.tasks, a.taskSvc, tc, a.audit, a.logger, emit, a.cfg.Todoist)
	a.logger.Info("todoist.enabled", "project_id", a.cfg.Todoist.ProjectID)
}

// todoistPollLoop runs the Todoist sync on a timer.
func (a *App) todoistPollLoop(ctx context.Context) {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := a.todoistHandler.pollAndSync()
			timer.Reset(next)
		}
	}
}

// startTodoistLoop launches the poll goroutine if the handler is initialized.
func (a *App) startTodoistLoop(parent context.Context) {
	if a.todoistHandler == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.todoistCancel = cancel
	a.wg.Go(func() { a.todoistPollLoop(ctx) })
}

// stopTodoistLoop cancels the running poll goroutine if any.
func (a *App) stopTodoistLoop() {
	if a.todoistCancel != nil {
		a.todoistCancel()
		a.todoistCancel = nil
	}
	a.todoistHandler = nil
}

// reloadTodoist tears down and (if enabled) re-creates the Todoist handler + poll loop.
func (a *App) reloadTodoist() {
	a.stopTodoistLoop()
	a.initTodoist(a.emit)
	a.startTodoistLoop(a.ctx)
}
