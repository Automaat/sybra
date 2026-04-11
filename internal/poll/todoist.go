package poll

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/logging"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/todoist"
)

// TaskCreator creates a new task. Matches TaskService.CreateTask signature.
type TaskCreator func(title, body, mode string) (task.Task, error)

// TodoistHandler syncs tasks between Todoist and Synapse.
type TodoistHandler struct {
	tasks      *task.Manager
	createTask TaskCreator
	client     *todoist.Client
	cfg        config.TodoistConfig
	audit      *audit.Logger
	logger     *slog.Logger
	emit       func(string, any)
	throttle   *logging.ErrorThrottle
}

// NewTodoistHandler creates a TodoistHandler.
func NewTodoistHandler(
	tasks *task.Manager,
	createTask TaskCreator,
	client *todoist.Client,
	al *audit.Logger,
	logger *slog.Logger,
	emit func(string, any),
	cfg config.TodoistConfig,
) *TodoistHandler {
	return &TodoistHandler{
		tasks:      tasks,
		createTask: createTask,
		client:     client,
		cfg:        cfg,
		audit:      al,
		logger:     logger,
		emit:       emit,
		throttle:   logging.NewErrorThrottle(),
	}
}

func (h *TodoistHandler) Name() string { return "todoist" }

func (h *TodoistHandler) Poll(_ context.Context) time.Duration {
	return h.PollAndSync()
}

// PollAndSync runs one import+completion cycle and returns the next poll interval.
func (h *TodoistHandler) PollAndSync() time.Duration {
	interval := time.Duration(h.cfg.PollSeconds) * time.Second

	imported, importErr := h.ImportNewTasks()
	h.throttle.Log(h.logger, "todoist.import", "import", importErr)

	completed, compErr := h.syncCompletions()
	h.throttle.Log(h.logger, "todoist.complete", "complete", compErr)

	if imported > 0 || completed > 0 {
		h.logger.Info("todoist.synced", "imported", imported, "completed", completed)
		h.emit(events.TodoistSynced, map[string]any{
			"imported":  imported,
			"completed": completed,
		})
	}

	return interval
}

// ImportNewTasks fetches active Todoist tasks and creates missing ones in Synapse.
func (h *TodoistHandler) ImportNewTasks() (int, error) {
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

		t, createErr := h.createTask(rt.Content, body, "headless")
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
		closeErr := h.client.CloseTask(t.TodoistID)
		h.throttle.Log(h.logger, "todoist.close", "close:"+t.TodoistID, closeErr,
			"task_id", t.ID, "todoist_id", t.TodoistID)
		if closeErr != nil {
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

func (h *TodoistHandler) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if h.audit == nil {
		return
	}
	if err := h.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		h.logger.Error("audit.log", "type", eventType, "err", err)
	}
}
