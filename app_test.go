package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setupApp(t *testing.T) *App {
	t.Helper()
	// Use os.MkdirTemp instead of t.TempDir() to avoid cleanup races
	// with background goroutines (TriageTask spawned by CreateTask).
	dir, err := os.MkdirTemp("", "synapse-test-tasks-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	logger := discardLogger()
	emit := func(string, any) {}
	tm := tmux.NewManager()
	logDir := filepath.Join(os.TempDir(), "synapse-test-logs")
	mgr := agent.NewManager(t.Context(), tm, emit, logger, logDir)

	return &App{
		tasks:    store,
		agents:   mgr,
		tasksDir: dir,
		logger:   logger,
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Logging:      config.LoggingConfig{Dir: t.TempDir()},
		TasksDir:     t.TempDir(),
		SkillsDir:    t.TempDir(),
		ProjectsDir:  t.TempDir(),
		ClonesDir:    t.TempDir(),
		WorktreesDir: t.TempDir(),
	}
}

func TestNewApp(t *testing.T) {
	cfg := testConfig(t)
	a := NewApp(discardLogger(), cfg)
	if a == nil {
		t.Fatal("NewApp returned nil")
	}
	if a.tasksDir != cfg.TasksDir {
		t.Errorf("tasksDir = %q, want %q", a.tasksDir, cfg.TasksDir)
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

func TestSyncFile(t *testing.T) {
	a := setupApp(t)
	a.repoDir = t.TempDir()

	srcDir := filepath.Join(a.repoDir, "sub")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "test.md")
	if err := os.WriteFile(srcFile, []byte("# hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstFile := filepath.Join(t.TempDir(), "out", "test.md")
	a.syncFile(srcFile, dstFile)

	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("dst not written: %v", err)
	}
	if string(data) != "# hello" {
		t.Errorf("content = %q, want %q", string(data), "# hello")
	}
}

func TestSyncFileMissingSrc(t *testing.T) {
	a := setupApp(t)
	dstFile := filepath.Join(t.TempDir(), "should-not-exist.md")
	a.syncFile("/nonexistent/file.md", dstFile)

	if _, err := os.Stat(dstFile); !os.IsNotExist(err) {
		t.Error("dst should not be created when src missing")
	}
}

func TestSyncDir(t *testing.T) {
	a := setupApp(t)

	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.md", "b.md", "c.txt"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("content-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Add a subdirectory that should be skipped
	if err := os.MkdirAll(filepath.Join(srcDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	a.syncDir(srcDir, dstDir)

	// Only .md files should be copied
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d files, want 2 (.md only)", len(entries))
	}
}

func TestSyncDirMissingSrc(t *testing.T) {
	a := setupApp(t)
	dstDir := filepath.Join(t.TempDir(), "should-not-exist")
	a.syncDir("/nonexistent/dir", dstDir)

	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Error("dst dir should not be created when src missing")
	}
}

func TestSyncSkillsNoRepoDir(t *testing.T) {
	a := setupApp(t)
	a.repoDir = ""
	// Should not panic; falls back to cwd
	a.syncSkills()
}

func TestSyncSkillsWithRepoDir(t *testing.T) {
	a := setupApp(t)

	repoDir := t.TempDir()
	a.repoDir = repoDir

	// Create source skills dir
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "skill.md"), []byte("# skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create orchestrator CLAUDE.md
	orchDir := filepath.Join(repoDir, "orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "CLAUDE.md"), []byte("# orchestrator"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not panic
	a.syncSkills()
}

func TestShutdown(t *testing.T) {
	a := setupApp(t)
	a.shutdown(t.Context())
}

func TestStartup(t *testing.T) {
	a := NewApp(discardLogger(), testConfig(t))
	if a.tasksDir == "" {
		t.Error("tasksDir should not be empty")
	}
}
