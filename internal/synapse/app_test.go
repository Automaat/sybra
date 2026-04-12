package synapse

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/task"

	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
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
	taskMgr := task.NewManager(store, nil)

	logger := discardLogger()
	emit := func(string, any) {}
	logDir := filepath.Join(os.TempDir(), "synapse-test-logs")
	mgr := agent.NewManager(t.Context(), emit, logger, logDir)

	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: mgr.HasRunningAgentForTask,
	})
	agentOrch := newAgentOrchestrator(taskMgr, nil, mgr, nil, logger, wm, nil)

	return &App{
		tasks:     taskMgr,
		agents:    mgr,
		tasksDir:  dir,
		logger:    logger,
		worktrees: wm,
		agentOrch: agentOrch,
	}
}

func setupTaskService(t *testing.T) (*TaskService, *App) {
	t.Helper()
	a := setupApp(t)
	var wg sync.WaitGroup

	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.SyncBuiltins(wfStore); err != nil {
		t.Fatal(err)
	}
	ta := &taskAdapter{tasks: a.tasks}
	aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
	engine := workflow.NewEngine(wfStore, ta, aa, a.logger)

	svc := &TaskService{
		tasks:          a.tasks,
		agents:         a.agents,
		workflowEngine: engine,
		worktrees:      a.worktrees,
		wg:             &wg,
		logger:         a.logger,
	}
	return svc, a
}

func setupPlanningService(t *testing.T) (*PlanningService, *TaskService, *App) {
	t.Helper()
	taskSvc, a := setupTaskService(t)
	planSvc := &PlanningService{
		engine: taskSvc.workflowEngine,
		tasks:  a.tasks,
		agents: a.agents,
	}
	return planSvc, taskSvc, a
}

func setupAgentService(t *testing.T) (*AgentService, *App) {
	t.Helper()
	a := setupApp(t)
	svc := &AgentService{
		agents: a.agents,
		logger: a.logger,
	}
	return svc, a
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

func TestOnAgentComplete_EmptyTaskID_NoCrash(t *testing.T) {
	// Orchestrator brain agents run with TaskID="" — feeding that into
	// UpdateRun/HandleAgentComplete/Get used to crash the handler with
	// "open .synapse/tasks/.md: no such file or directory" because the
	// empty ID was joined to the tasks dir. Verify the short-circuit.
	a := setupApp(t)

	// Pre-existing task that must NOT be touched: an empty-id call must
	// not accidentally rewrite a task file (the original bug joined "" to
	// the tasks dir as ".md" — the path it would have hit is here too).
	other, err := a.tasks.Create("Other task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	otherStat, err := os.Stat(other.FilePath)
	if err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{
		ID:        "orch-agent",
		TaskID:    "",
		Mode:      "interactive",
		StartedAt: other.CreatedAt,
	}

	// Should not panic, and should not touch any task file. The historical
	// bug created/touched ".md" in tasksDir; assert no such file exists.
	a.onAgentComplete(ag)

	if _, err := os.Stat(filepath.Join(a.tasksDir, ".md")); !os.IsNotExist(err) {
		t.Errorf("expected no .md file in tasks dir, got err=%v", err)
	}
	otherStat2, err := os.Stat(other.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !otherStat.ModTime().Equal(otherStat2.ModTime()) {
		t.Errorf("unrelated task file was rewritten: mtime %v -> %v", otherStat.ModTime(), otherStat2.ModTime())
	}
}

func TestNewApp(t *testing.T) {
	cfg := testConfig(t)
	a := NewApp(discardLogger(), &slog.LevelVar{}, cfg)
	if a == nil {
		t.Fatal("NewApp returned nil")
	}
	if a.tasksDir != cfg.TasksDir {
		t.Errorf("tasksDir = %q, want %q", a.tasksDir, cfg.TasksDir)
	}
}

func TestListTasksEmpty(t *testing.T) {
	svc, _ := setupTaskService(t)
	tasks, err := svc.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want 0", len(tasks))
	}
}

func TestCreateAndGetTask(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("test title", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "test title" {
		t.Errorf("Title = %q, want %q", created.Title, "test title")
	}

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestUpdateTask(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("update me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.UpdateTask(created.ID, map[string]any{"status": "done"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "done" {
		t.Errorf("Status = %q, want %q", updated.Status, "done")
	}
}

func TestListTasksAfterCreate(t *testing.T) {
	svc, _ := setupTaskService(t)

	for _, title := range []string{"one", "two", "three"} {
		if _, err := svc.CreateTask(title, "", "headless"); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := svc.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Errorf("got %d tasks, want 3", len(tasks))
	}
}

func TestGetTaskNotFound(t *testing.T) {
	svc, _ := setupTaskService(t)
	_, err := svc.GetTask("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

// TestStartAgentRejectsMissingProject verifies the orchestrator refuses to
// spawn an agent when the task has no project_id, preventing the agent from
// mutating Synapse's own working directory (the class of bug that caused
// branch changes in the main repo).
func TestStartAgentRejectsMissingProject(t *testing.T) {
	taskSvc, a := setupTaskService(t)

	created, err := taskSvc.CreateTask("agent task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.StartAgent(created.ID, "headless", "test prompt")
	if err == nil {
		t.Fatal("expected error: task without project_id must be rejected")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Errorf("expected project_id error, got: %v", err)
	}
}

func TestStartAgentTaskNotFound(t *testing.T) {
	a := setupApp(t)
	_, err := a.StartAgent("nonexistent", "headless", "prompt")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

// runTestAgent bypasses the orchestrator (which requires a project) and spawns
// an agent directly in a temp dir. Used by lifecycle tests that only care
// about agent state machinery, not worktree integration.
func runTestAgent(t *testing.T, a *App, taskID, title string) *agent.Agent {
	t.Helper()
	dir, err := os.MkdirTemp("", "synapse-app-agent-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: taskID,
		Name:   title,
		Mode:   "headless",
		Prompt: "test",
		Dir:    dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func TestStopAgent(t *testing.T) {
	taskSvc, a := setupTaskService(t)
	agentSvc := &AgentService{agents: a.agents, logger: a.logger}

	created, err := taskSvc.CreateTask("stop task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag := runTestAgent(t, a, created.ID, "stop task")

	if err := agentSvc.StopAgent(ag.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStopAgentNotFound(t *testing.T) {
	svc, _ := setupAgentService(t)
	err := svc.StopAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListAgentsEmpty(t *testing.T) {
	svc, _ := setupAgentService(t)
	agents := svc.ListAgents()
	if len(agents) != 0 {
		t.Errorf("got %d agents, want 0", len(agents))
	}
}

func TestDiscoverAgents(t *testing.T) {
	svc, _ := setupAgentService(t)
	agents := svc.DiscoverAgents()
	_ = agents
}

func TestGetAgentOutput(t *testing.T) {
	taskSvc, a := setupTaskService(t)
	agentSvc := &AgentService{agents: a.agents, logger: a.logger}

	created, err := taskSvc.CreateTask("output task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag := runTestAgent(t, a, created.ID, "output task")

	events, err := agentSvc.GetAgentOutput(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		events = []agent.StreamEvent{}
	}
	_ = events
}

func TestGetAgentOutputNotFound(t *testing.T) {
	svc, _ := setupAgentService(t)
	_, err := svc.GetAgentOutput("nonexistent")
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

func TestSyncDirRemovesOrphans(t *testing.T) {
	a := setupApp(t)

	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "keep.md"), []byte("# keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate orphan file that should be removed.
	if err := os.WriteFile(filepath.Join(dstDir, "orphan.md"), []byte("# orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	a.syncDir(srcDir, dstDir)

	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d files, want 1 (orphan removed)", len(entries))
	}
	if entries[0].Name() != "keep.md" {
		t.Errorf("expected keep.md, got %s", entries[0].Name())
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
	a.Shutdown(t.Context())
}

func TestStartup(t *testing.T) {
	a := NewApp(discardLogger(), &slog.LevelVar{}, testConfig(t))
	if a.tasksDir == "" {
		t.Error("tasksDir should not be empty")
	}
}

func TestResolvePermission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		taskPerm *bool
		cfgPerm  *bool
		want     bool
	}{
		{"task false overrides config true", task.Ptr(false), task.Ptr(true), false},
		{"task true overrides config false", task.Ptr(true), task.Ptr(false), true},
		{"task nil falls back to config false", nil, task.Ptr(false), false},
		{"task nil falls back to config true", nil, task.Ptr(true), true},
		{"task nil config nil defaults true", nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tk := task.Task{RequirePermissions: tt.taskPerm}
			var cfg *config.Config
			if tt.cfgPerm != nil {
				cfg = &config.Config{Agent: config.AgentDefaults{RequirePermissions: tt.cfgPerm}}
			}
			if got := resolvePermission(tk, cfg); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveExecutionDebugAlwaysRequiresPermissions(t *testing.T) {
	t.Parallel()
	tk := task.Task{TaskType: task.TaskTypeDebug, RequirePermissions: task.Ptr(false)}
	// TaskTypeDebug hardcodes requirePerm=true regardless of task field.
	_, _, requirePerm, _ := resolveExecution(tk, "headless", "", nil)
	if !requirePerm {
		t.Error("debug task should always require permissions")
	}
}
