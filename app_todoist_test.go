package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/todoist"
)

// fakeTodoistServer serves one active task on /tasks and returns 204 on /close.
func fakeTodoistServer(t *testing.T, task todoist.Task) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tasks":
			_ = json.NewEncoder(w).Encode(struct {
				Results    []todoist.Task `json:"results"`
				NextCursor *string        `json:"next_cursor"`
			}{Results: []todoist.Task{task}})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestTodoistImport_SetsDefaultProjectID verifies that when DefaultProjectID
// is configured, imported Todoist tasks get that project_id set. Regression
// test for Todoist-sourced tasks landing in Synapse with empty project_id,
// which breaks PR linking in the frontend and pr-monitor dispatch.
func TestTodoistImport_SetsDefaultProjectID(t *testing.T) {
	remote := todoist.Task{
		ID:          "todo-123",
		Content:     "Add dark mode toggle",
		Description: "from the user",
		URL:         "https://todoist.com/showTask?id=todo-123",
	}
	srv := fakeTodoistServer(t, remote)
	t.Cleanup(srv.Close)

	taskSvc, a := setupTaskService(t)
	cfg := config.TodoistConfig{
		Enabled:          true,
		APIToken:         "tok",
		ProjectID:        "tdProj",
		DefaultProjectID: "Automaat/synapse",
		PollSeconds:      60,
	}
	client := todoist.NewClientWith(cfg.APIToken, http.DefaultClient, srv.URL)

	h := newTodoistHandler(
		a.tasks,
		taskSvc,
		client,
		nil,
		a.logger,
		func(string, any) {},
		cfg,
	)

	imported, err := h.ImportNewTasks()
	if err != nil {
		t.Fatalf("importNewTasks: %v", err)
	}
	if imported != 1 {
		t.Fatalf("imported = %d, want 1", imported)
	}

	tasks, err := a.tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	got := tasks[0]
	if got.TodoistID != "todo-123" {
		t.Errorf("todoist_id = %q, want todo-123", got.TodoistID)
	}
	if got.ProjectID != "Automaat/synapse" {
		t.Errorf("project_id = %q, want Automaat/synapse", got.ProjectID)
	}
	if got.Title != "Add dark mode toggle" {
		t.Errorf("title = %q, want Add dark mode toggle", got.Title)
	}
}

// TestTodoistImport_EmptyDefaultProjectIDLeavesUnset is the negative case:
// when DefaultProjectID is empty, imported tasks must keep ProjectID empty
// (so we don't accidentally assign all Todoist tasks to some default repo).
func TestTodoistImport_EmptyDefaultProjectIDLeavesUnset(t *testing.T) {
	remote := todoist.Task{
		ID:      "todo-456",
		Content: "Something without a project",
	}
	srv := fakeTodoistServer(t, remote)
	t.Cleanup(srv.Close)

	taskSvc, a := setupTaskService(t)
	cfg := config.TodoistConfig{
		Enabled:   true,
		APIToken:  "tok",
		ProjectID: "tdProj",
		// DefaultProjectID intentionally empty.
		PollSeconds: 60,
	}
	client := todoist.NewClientWith(cfg.APIToken, http.DefaultClient, srv.URL)

	h := newTodoistHandler(
		a.tasks,
		taskSvc,
		client,
		nil,
		a.logger,
		func(string, any) {},
		cfg,
	)

	if _, err := h.ImportNewTasks(); err != nil {
		t.Fatalf("importNewTasks: %v", err)
	}

	tasks, err := a.tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if got := tasks[0].ProjectID; got != "" {
		t.Errorf("project_id = %q, want empty", got)
	}
	if got := tasks[0].TodoistID; got != "todo-456" {
		t.Errorf("todoist_id = %q, want todo-456", got)
	}
}
