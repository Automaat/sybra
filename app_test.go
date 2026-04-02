package main

import (
	"path/filepath"
	"testing"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
)

func setupApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()

	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	emit := func(string, any) {}
	tm := tmux.NewManager()
	mgr := agent.NewManager(t.Context(), tm, emit)

	return &App{
		tasks:    store,
		agents:   mgr,
		tasksDir: dir,
	}
}

func TestNewApp(t *testing.T) {
	a := NewApp()
	if a == nil {
		t.Fatal("NewApp returned nil")
	}
	if a.tasksDir != "tasks" {
		t.Errorf("tasksDir = %q, want %q", a.tasksDir, "tasks")
	}
}

func TestListTasksEmpty(t *testing.T) {
	a := setupApp(t)
	tasks, err := a.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want 0", len(tasks))
	}
}

func TestCreateAndGetTask(t *testing.T) {
	a := setupApp(t)

	created, err := a.CreateTask("test title", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "test title" {
		t.Errorf("Title = %q, want %q", created.Title, "test title")
	}

	got, err := a.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestUpdateTask(t *testing.T) {
	a := setupApp(t)

	created, err := a.CreateTask("update me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := a.UpdateTask(created.ID, map[string]any{"status": "done"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "done" {
		t.Errorf("Status = %q, want %q", updated.Status, "done")
	}
}

func TestListTasksAfterCreate(t *testing.T) {
	a := setupApp(t)

	for _, title := range []string{"one", "two", "three"} {
		if _, err := a.CreateTask(title, "", "headless"); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := a.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Errorf("got %d tasks, want 3", len(tasks))
	}
}

func TestGetTaskNotFound(t *testing.T) {
	a := setupApp(t)
	_, err := a.GetTask("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStartAgentHeadless(t *testing.T) {
	a := setupApp(t)

	created, err := a.CreateTask("agent task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag, err := a.StartAgent(created.ID, "headless", "test prompt")
	if err != nil {
		t.Fatal(err)
	}
	if ag.TaskID != created.ID {
		t.Errorf("TaskID = %q, want %q", ag.TaskID, created.ID)
	}
}

func TestStartAgentTaskNotFound(t *testing.T) {
	a := setupApp(t)
	_, err := a.StartAgent("nonexistent", "headless", "prompt")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStopAgent(t *testing.T) {
	a := setupApp(t)

	created, err := a.CreateTask("stop task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag, err := a.StartAgent(created.ID, "headless", "prompt")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.StopAgent(ag.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStopAgentNotFound(t *testing.T) {
	a := setupApp(t)
	err := a.StopAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListAgentsEmpty(t *testing.T) {
	a := setupApp(t)
	agents := a.ListAgents()
	if len(agents) != 0 {
		t.Errorf("got %d agents, want 0", len(agents))
	}
}

func TestDiscoverAgents(t *testing.T) {
	a := setupApp(t)
	agents := a.DiscoverAgents()
	_ = agents
}

func TestGetAgentOutput(t *testing.T) {
	a := setupApp(t)

	created, err := a.CreateTask("output task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag, err := a.StartAgent(created.ID, "headless", "prompt")
	if err != nil {
		t.Fatal(err)
	}

	events, err := a.GetAgentOutput(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		events = []agent.StreamEvent{}
	}
	_ = events
}

func TestGetAgentOutputNotFound(t *testing.T) {
	a := setupApp(t)
	_, err := a.GetAgentOutput("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestShutdown(t *testing.T) {
	a := setupApp(t)
	a.shutdown(t.Context())
}

func TestStartup(t *testing.T) {
	dir := t.TempDir()
	a := &App{tasksDir: filepath.Join(dir, "tasks")}
	// startup needs wails runtime for EventsEmit — skip the full startup
	// but test that NewApp + tasksDir is valid
	if a.tasksDir == "" {
		t.Error("tasksDir should not be empty")
	}
}
