package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	eventnames "github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"

	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func setupApp(t *testing.T) *App {
	t.Helper()
	// Use os.MkdirTemp instead of t.TempDir() to avoid cleanup races
	// with background goroutines (TriageTask spawned by CreateTask).
	dir, err := os.MkdirTemp("", "sybra-test-tasks-*")
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
	logDir := filepath.Join(os.TempDir(), "sybra-test-logs")
	mgr := newTestAgentManager(t, t.Context(), emit, logger, logDir)

	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: mgr.HasRunningAgentForTask,
	})
	agentOrch := agentorch.New(taskMgr, nil, mgr, nil, logger, wm, nil)

	return &App{
		tasks:     taskMgr,
		agents:    mgr,
		tasksDir:  dir,
		logger:    logger,
		worktrees: wm,
		agentOrch: agentOrch,
	}
}

func TestInitAgentManagerEmitsDegradedWhenSurvivalDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "agents"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Agent.Provider = "claude"
	a := &App{
		cfg:      cfg,
		logger:   discardLogger(),
		logDir:   t.TempDir(),
		agentSvc: &AgentService{},
	}
	var emitted []string
	err := a.initAgentManager(t.Context(), func(event string, _ any) {
		emitted = append(emitted, event)
	})
	if err != nil {
		t.Fatalf("initAgentManager: %v", err)
	}
	if a.agentSvc.approval != nil {
		t.Cleanup(func() { _ = a.agentSvc.approval.Shutdown(context.Background()) })
	}
	if a.agents == nil {
		t.Fatal("manager was not initialized")
	}
	if a.agents.DefaultProvider() != "claude" {
		t.Fatalf("DefaultProvider = %q, want claude", a.agents.DefaultProvider())
	}
	if !slices.Contains(emitted, eventnames.StartupDegraded) {
		t.Fatalf("expected %s event, got %v", eventnames.StartupDegraded, emitted)
	}
}

func TestInitAgentManagerClosesApprovalServerOnFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Agent.Provider = "unknown"
	cfg.Agent.ApprovalPort = port
	a := &App{
		cfg:      cfg,
		logger:   discardLogger(),
		logDir:   t.TempDir(),
		agentSvc: &AgentService{},
	}

	if err := a.initAgentManager(t.Context(), func(string, any) {}); err == nil {
		t.Fatal("expected initAgentManager to fail")
	}

	rebound, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("approval server still bound port %d: %v", port, err)
	}
	if err := rebound.Close(); err != nil {
		t.Fatal(err)
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

func TestNewApp(t *testing.T) {
	cfg := testConfig(t)
	a := NewApp(discardLogger(), &slog.LevelVar{}, cfg)
	if a == nil {
		t.Fatal("NewApp returned nil")
		return
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
	// Use a TaskService without a workflow engine so CreateTask doesn't
	// spawn a triage goroutine that races with UpdateTask's running-agent check.
	a := setupApp(t)
	var wg sync.WaitGroup
	svc := &TaskService{
		tasks:     a.tasks,
		agents:    a.agents,
		worktrees: a.worktrees,
		wg:        &wg,
		logger:    a.logger,
	}

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
// mutating Sybra's own working directory (the class of bug that caused
// branch changes in the main repo).
func TestStartAgentRejectsMissingProject(t *testing.T) {
	taskSvc, a := setupTaskService(t)

	created, err := taskSvc.CreateTask("agent task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.StartAgent(created.ID, "headless", "test prompt", false)
	if err == nil {
		t.Fatal("expected error: task without project_id must be rejected")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Errorf("expected project_id error, got: %v", err)
	}
}

func TestStartAgentTaskNotFound(t *testing.T) {
	a := setupApp(t)
	_, err := a.StartAgent("nonexistent", "headless", "prompt", false)
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

// runTestAgent bypasses the orchestrator (which requires a project) and spawns
// an agent directly in a temp dir. Used by lifecycle tests that only care
// about agent state machinery, not worktree integration.
func runTestAgent(t *testing.T, a *App, taskID, title string) *agent.Agent {
	t.Helper()
	dir, err := os.MkdirTemp("", "sybra-app-agent-*")
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

func TestShutdown(t *testing.T) {
	a := setupApp(t)
	a.Shutdown(t.Context())
}

func TestShutdownBeforeStartup(t *testing.T) {
	a := NewApp(discardLogger(), &slog.LevelVar{}, testConfig(t))
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
			if got := agentorch.ResolvePermission(tk, cfg); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveExecutionDebugAlwaysRequiresPermissions(t *testing.T) {
	t.Parallel()
	tk := task.Task{TaskType: task.TaskTypeDebug, RequirePermissions: task.Ptr(false)}
	// TaskTypeDebug hardcodes requirePerm=true regardless of task field.
	_, _, requirePerm, _ := agentorch.ResolveExecution(tk, "headless", "", nil)
	if !requirePerm {
		t.Error("debug task should always require permissions")
	}
}
