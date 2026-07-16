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
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/config"
	eventnames "github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"

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

func setupManualQueueApp(t *testing.T, taskDir, queueDir string, maxConcurrent int) *App {
	t.Helper()
	if taskDir == "" {
		var err error
		taskDir, err = os.MkdirTemp("", "sybra-manual-queue-tasks-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(taskDir) })
	}
	if queueDir == "" {
		queueDir = t.TempDir()
	}

	store, err := task.NewStore(taskDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	fakebin := t.TempDir()
	fakeClaude := filepath.Join(fakebin, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/usr/bin/env bash\n"+
		"trap 'exit 0' TERM INT\n"+
		"printf '{\"type\":\"system\",\"session_id\":\"fake-session\"}\\n'\n"+
		"sleep 5\n"+
		"printf '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"fake-session\",\"result\":\"done\",\"total_cost_usd\":0.01,\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}\\n'\n"),
		0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))

	logger := discardLogger()
	mgr, err := agent.NewManager(t.Context(), func(string, any) {}, logger, t.TempDir(), agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{DefaultProvider: "claude", MaxConcurrent: maxConcurrent},
		SandboxHome: func(string) (string, error) {
			return t.TempDir(), nil
		},
	})
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.ShutdownWithGrace(2 * time.Second) })
	q, err := agentqueue.New(queueDir, agentqueue.Options{}, logger)
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	noPermissions := false
	cfg := config.DefaultConfig()
	cfg.Agent.ResearchMachineDir = t.TempDir()
	cfg.Agent.RequirePermissions = &noPermissions

	orch := agentorch.New(taskMgr, nil, mgr, nil, logger, nil, cfg)
	orch.SetQueue(q)
	orch.SetContext(t.Context())

	return &App{
		ctx:           t.Context(),
		tasks:         taskMgr,
		agents:        mgr,
		agentQueue:    q,
		agentOrch:     orch,
		logger:        logger,
		cfg:           cfg,
		dispatchNudge: make(chan struct{}, 1),
		orchSvc:       &OrchestratorService{},
	}
}

func createResearchTaskWithPriority(t *testing.T, tasks *task.Manager, title string, priority task.Priority) task.Task {
	t.Helper()
	created, err := tasks.Create(title, "", "headless")
	if err != nil {
		t.Fatalf("task Create(%q): %v", title, err)
	}
	updated, err := tasks.Update(created.ID, task.Update{
		TaskType: task.Ptr(task.TaskTypeResearch),
		Priority: task.Ptr(priority),
	})
	if err != nil {
		t.Fatalf("task Update(%q): %v", title, err)
	}
	return updated
}

func waitForTaskAgent(t *testing.T, mgr *agent.Manager, taskID string) *agent.Agent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, ag := range mgr.ListAgents() {
			if ag != nil && ag.TaskID == taskID {
				return ag
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for agent registration for task %s", taskID)
	return nil
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

// fakeTriageClassifier is a deterministic triage.Classifier stand-in for
// tests: it keeps the task's existing title and always routes it to a small
// chore, headless, todo — no LLM call involved. Wired into setupTaskService
// so the builtin simple-task-plan workflow's classify_task step completes
// the way its old run_agent predecessor did in tests, without a real
// provider on the other end.
type fakeTriageClassifier struct{}

func (fakeTriageClassifier) Classify(_ context.Context, t task.Task, _ []project.Project) (triage.Verdict, error) {
	return triage.Verdict{
		Title: t.Title,
		Tags:  []string{"chore", "small"},
		Size:  "small",
		Type:  "chore",
		Mode:  "headless",
	}, nil
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
	// Bind the test context so a background workflow (spawned by CreateTask)
	// unwinds when the test ends — otherwise classify_task's retry backoff can
	// outlive the task dir's cleanup and leak a sleeping goroutine.
	engine.SetContext(t.Context())
	// The triage step is now a deterministic classify_task step (no agent
	// dispatch); wire a fake classifier so simple-task-plan's triage step
	// completes without a real LLM call, mirroring how run_agent steps used
	// to complete instantly against the mocked agent manager.
	engine.SetTaskClassifier(&taskClassifierAdapter{
		tasks:      a.tasks,
		classifier: fakeTriageClassifier{},
	})

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

func TestGetPressureGateWiresDiskReclaimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	a := setupApp(t)
	cfg := config.DefaultConfig()
	cfg.Logging.Dir = filepath.Join(home, "logs")
	cfg.TasksDir = filepath.Join(home, "tasks")
	cfg.SkillsDir = filepath.Join(home, "skills")
	cfg.ProjectsDir = filepath.Join(home, "projects")
	cfg.ClonesDir = filepath.Join(home, "clones")
	cfg.WorktreesDir = filepath.Join(home, "worktrees")
	a.cfg = cfg
	if gate := a.getPressureGate(); gate == nil {
		t.Fatal("getPressureGate() = nil, want gate")
	}
	if a.diskReclaimer == nil {
		t.Fatal("diskReclaimer = nil, want shared reclaimer wired with pressure gate")
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

func TestStartAgentQueuedManualDoesNotRegisterLiveAgent(t *testing.T) {
	a := setupManualQueueApp(t, "", "", 1)

	blocker := createResearchTaskWithPriority(t, a.tasks, "blocker", task.PriorityMedium)
	blockerAgent, err := a.agentOrch.StartAgent(blocker.ID, "headless", "hold", false, false)
	if err != nil {
		t.Fatalf("StartAgent(blocker): %v", err)
	}
	t.Cleanup(func() { _ = a.agents.StopAgent(blockerAgent.ID) })

	queuedTask := createResearchTaskWithPriority(t, a.tasks, "queued manual", task.PriorityHigh)
	queued, err := a.StartAgent(queuedTask.ID, "headless", "ship it", true)
	if err != nil {
		t.Fatalf("StartAgent(queuedTask): %v", err)
	}
	if queued.State != agent.StateQueued {
		t.Fatalf("queued State = %q, want %q", queued.State, agent.StateQueued)
	}
	if _, err := a.agents.GetAgent(queued.ID); err == nil {
		t.Fatalf("queued agent %q must not be registered live", queued.ID)
	}
	if got := a.agents.RunningCount(); got != 1 {
		t.Fatalf("RunningCount = %d, want 1", got)
	}
	snap := a.agentQueue.Snapshot()
	if len(snap) != 1 || snap[0].TaskID != queuedTask.ID || !snap[0].Manual {
		t.Fatalf("queue snapshot = %+v, want single manual item for %s", snap, queuedTask.ID)
	}
}

func TestManualQueueDrainPriorityAndWorkflowPreservation(t *testing.T) {
	a := setupManualQueueApp(t, "", "", 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.agents.QueueNudge():
				a.queueDrainPass(ctx)
			}
		}
	}()
	defer func() {
		cancel()
		<-done
	}()

	blocker := createResearchTaskWithPriority(t, a.tasks, "blocker", task.PriorityMedium)
	_, err := a.agentOrch.StartAgent(blocker.ID, "headless", "hold", false, false)
	if err != nil {
		t.Fatalf("StartAgent(blocker): %v", err)
	}

	high := createResearchTaskWithPriority(t, a.tasks, "manual high", task.PriorityHigh)
	low := createResearchTaskWithPriority(t, a.tasks, "manual low", task.PriorityLow)
	if _, err := a.StartAgent(high.ID, "headless", "high", false); err != nil {
		t.Fatalf("StartAgent(high): %v", err)
	}
	if _, err := a.StartAgent(low.ID, "headless", "low", false); err != nil {
		t.Fatalf("StartAgent(low): %v", err)
	}
	workflowTask := createResearchTaskWithPriority(t, a.tasks, "workflow token", task.PriorityUrgent)
	a.agentQueue.Offer(agentqueue.Item{
		TaskID:   workflowTask.ID,
		Role:     string(agent.RoleImplementation),
		Priority: task.PriorityUrgent,
		Status:   task.StatusTodo,
	})

	a.agents.KillAgentsForTask(blocker.ID, 5*time.Second)
	start := time.Now()
	waitForTaskAgent(t, a.agents, high.ID)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("manual queue drain took %s, want under 1s after queue nudge", elapsed)
	}

	for _, ag := range a.agents.ListAgents() {
		if ag != nil && ag.TaskID == low.ID {
			t.Fatalf("low-priority manual task started before high-priority task: %+v", ag)
		}
	}
	snap := a.agentQueue.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("queue snapshot after first drain = %+v, want workflow token + low manual", snap)
	}
	var sawWorkflow, sawLow bool
	for _, it := range snap {
		if it.TaskID == workflowTask.ID && !it.Manual {
			sawWorkflow = true
		}
		if it.TaskID == low.ID && it.Manual {
			sawLow = true
		}
	}
	if !sawWorkflow || !sawLow {
		t.Fatalf("queue snapshot after first drain = %+v, want workflow token preserved and low manual still queued", snap)
	}
	a.agents.KillAgentsForTask(high.ID, 5*time.Second)
}

func TestManualQueueReloadDrainsAfterRestart(t *testing.T) {
	taskDir := t.TempDir()
	queueDir := t.TempDir()

	first := setupManualQueueApp(t, taskDir, queueDir, 1)
	blocker := createResearchTaskWithPriority(t, first.tasks, "blocker", task.PriorityMedium)
	blockerAgent, err := first.agentOrch.StartAgent(blocker.ID, "headless", "hold", false, false)
	if err != nil {
		t.Fatalf("StartAgent(blocker): %v", err)
	}
	queuedTask := createResearchTaskWithPriority(t, first.tasks, "restart queued", task.PriorityHigh)
	if _, err := first.StartAgent(queuedTask.ID, "headless", "after restart", false); err != nil {
		t.Fatalf("StartAgent(queuedTask): %v", err)
	}
	if err := first.agents.StopAgent(blockerAgent.ID); err != nil {
		t.Fatalf("StopAgent(blocker): %v", err)
	}

	restored := setupManualQueueApp(t, taskDir, queueDir, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		restored.orchestratorLoop(ctx)
	}()
	waitForTaskAgent(t, restored.agents, queuedTask.ID)
	restored.agents.KillAgentsForTask(queuedTask.ID, 5*time.Second)
	cancel()
	<-done
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
