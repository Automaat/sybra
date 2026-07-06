package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	eventnames "github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/project"
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

func TestOnAgentComplete_EmptyTaskID_NoCrash(t *testing.T) {
	// Orchestrator brain agents run with TaskID="" — feeding that into
	// UpdateRun/HandleAgentComplete/Get used to crash the handler with
	// "open .sybra/tasks/.md: no such file or directory" because the
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
	h := &AgentCompletionHandler{
		DomainHandler: DomainHandler{logger: a.logger},
		tasks:         a.tasks,
		worktrees:     a.worktrees,
	}
	h.OnComplete(ag)

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

func setupFixReviewPushTest(t *testing.T) (h *AgentCompletionHandler, taskMgr *task.Manager, barePath, src string) {
	t.Helper()
	home, err := os.MkdirTemp("", "sybra-fix-review-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	tasksDir := filepath.Join(home, "tasks")
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr = task.NewManager(taskStore, nil)

	projStore, err := project.NewStore(filepath.Join(home, "projects"), filepath.Join(home, "clones"))
	if err != nil {
		t.Fatal(err)
	}

	src = initFixReviewSourceRepo(t)
	barePath = filepath.Join(home, "clones", "testowner", "testrepo.git")
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.CloneBare(context.Background(), src, barePath); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	projYAML := `id: testowner/testrepo
name: testrepo
owner: testowner
repo: testrepo
url: ` + src + `
clone_path: ` + barePath + `
type: pet
created_at: 2025-01-01T00:00:00Z
updated_at: 2025-01-01T00:00:00Z
`
	projFile := filepath.Join(home, "projects", "testowner--testrepo.yaml")
	if err := os.WriteFile(projFile, []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := discardLogger()
	wm := worktree.New(worktree.Config{
		WorktreesDir: filepath.Join(home, "worktrees"),
		Projects:     projStore,
		Tasks:        taskMgr,
		Logger:       logger,
		PRBranchResolver: func(repo string, prNumber int) (string, error) {
			return project.DefaultBranch(context.Background(), barePath)
		},
	})

	h = &AgentCompletionHandler{
		DomainHandler: DomainHandler{logger: logger},
		tasks:         taskMgr,
		worktrees:     wm,
	}
	return h, taskMgr, barePath, src
}

func initFixReviewSourceRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@test.com"},
		{"-C", dir, "config", "user.name", "Test"},
		{"-C", dir, "config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", dir, "add", "."},
		{"-C", dir, "commit", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestOnAgentComplete_FixReviewPushesBranch(t *testing.T) {
	h, taskMgr, barePath, _ := setupFixReviewPushTest(t)
	branch, err := project.DefaultBranch(context.Background(), barePath)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	tk, err := taskMgr.Create("fix pr", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := taskMgr.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("testowner/testrepo"),
		PRNumber:  task.Ptr(42),
		Status:    task.Ptr(task.StatusInReview),
	})
	if err != nil {
		t.Fatal(err)
	}
	tk = updated

	wtPath, err := h.worktrees.PrepareForFix(context.Background(), tk, 42)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}

	for _, args := range [][]string{
		{"-C", wtPath, "config", "user.email", "test@test.com"},
		{"-C", wtPath, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("# updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "add", "."},
		{"-C", wtPath, "commit", "-m", "fix(review): update pr"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	localHead, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("local head: %v", err)
	}

	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID:   "agent-1",
		Role:      string(agent.RoleFixReview),
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	h.OnComplete(&agent.Agent{
		ID:        "agent-1",
		TaskID:    tk.ID,
		Mode:      "headless",
		Name:      agent.RoleFixReview.AgentName(tk.Title),
		StartedAt: time.Now(),
	})

	remoteHead, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", barePath, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		t.Fatalf("remote head: %v", err)
	}
	if got, want := strings.TrimSpace(string(remoteHead)), strings.TrimSpace(string(localHead)); got != want {
		t.Fatalf("remote head = %s, want %s", got, want)
	}
}

// TestOnAgentComplete_FixReviewHoldParksForHuman locks the review-hold branch of
// the manual fix-review path: the task is parked in human-required for the user
// to verify and submit the pending review, rather than following the auto-push
// path (which never touches status). The push decision is owned by the agent per
// the configured mode, so Sybra runs no force-push here.
func TestOnAgentComplete_FixReviewHoldParksForHuman(t *testing.T) {
	h, taskMgr, _, _ := setupFixReviewPushTest(t)
	h.cfg = &config.Config{ReviewHold: config.ReviewHoldConfig{
		Enabled: true, Mode: config.ReviewHoldModeHold,
	}}

	tk, err := taskMgr.Create("fix pr", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	tk, err = taskMgr.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("testowner/testrepo"),
		PRNumber:  task.Ptr(42),
		Status:    task.Ptr(task.StatusInReview),
	})
	if err != nil {
		t.Fatal(err)
	}

	wtPath, err := h.worktrees.PrepareForFix(context.Background(), tk, 42)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "config", "user.email", "test@test.com"},
		{"-C", wtPath, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("# updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "add", "."},
		{"-C", wtPath, "commit", "-m", "fix(review): update pr"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-1", Role: string(agent.RoleFixReview), Mode: "headless",
		State: string(agent.StateRunning), StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	h.OnComplete(&agent.Agent{
		ID:        "agent-1",
		TaskID:    tk.ID,
		Mode:      "headless",
		Name:      agent.RoleFixReview.AgentName(tk.Title),
		StartedAt: time.Now(),
	})

	got, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
}

// setupDivergedFixReviewPush prepares a fix-review worktree whose branch has
// genuinely diverged from its remote tracking ref (neither is an ancestor of
// the other) — the exact condition project.PushSync now refuses to force
// through. Mirrors internal/project's own PushSync divergence tests: seed a
// push, then rewrite local history so it no longer contains the tracking
// ref's tip.
func setupDivergedFixReviewPush(t *testing.T) (h *AgentCompletionHandler, taskMgr *task.Manager, tk task.Task) {
	t.Helper()
	h, taskMgr, barePath, src := setupFixReviewPushTest(t)
	branch, err := project.DefaultBranch(context.Background(), barePath)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	// src has its default branch checked out; without this, any push to it
	// (the seed push below and the fix-review push under test) is rejected
	// with "refusing to update checked out branch" regardless of divergence.
	if out, err := exec.Command("git", "-C", src, "config", "receive.denyCurrentBranch", "updateInstead").CombinedOutput(); err != nil {
		t.Fatalf("config denyCurrentBranch: %v: %s", err, out)
	}

	tk, err = taskMgr.Create("fix pr", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	tk, err = taskMgr.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("testowner/testrepo"),
		PRNumber:  task.Ptr(42),
		Status:    task.Ptr(task.StatusInReview),
	})
	if err != nil {
		t.Fatal(err)
	}

	wtPath, err := h.worktrees.PrepareForFix(context.Background(), tk, 42)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "config", "user.email", "test@test.com"},
		{"-C", wtPath, "config", "user.name", "Test"},
		{"-C", wtPath, "config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	gitCommit := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"-C", wtPath, "add", "."},
			{"-C", wtPath, "commit", "-m", "fix(review): " + content},
		} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	gitCommit("one")
	if err := project.PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Rewrite history locally so HEAD diverges from the remote tracking ref
	// PushSync compares against — same technique as
	// TestPushSync_DivergenceReturnsErrorNoForce in internal/project.
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", "HEAD~1").CombinedOutput(); err != nil {
		t.Fatalf("reset: %v: %s", err, out)
	}
	gitCommit("two-prime")

	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID:   "agent-1",
		Role:      string(agent.RoleFixReview),
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return h, taskMgr, tk
}

// TestOnAgentComplete_FixReviewPushDivergedRecoversViaAgent proves the core
// "never force-push" fix for the fix-review backstop: when the push hits a
// genuine divergence, Sybra must not force through it and must not strand the
// fix — it hands off to the wired conflict-recovery callback (the same
// autonomous agent-driven merge+push used elsewhere) instead.
func TestOnAgentComplete_FixReviewPushDivergedRecoversViaAgent(t *testing.T) {
	h, taskMgr, tk := setupDivergedFixReviewPush(t)
	var recoveredTaskID string
	h.conflictRecovery = func(taskID string) bool {
		recoveredTaskID = taskID
		return true
	}

	h.OnComplete(&agent.Agent{
		ID:        "agent-1",
		TaskID:    tk.ID,
		Mode:      "headless",
		Name:      agent.RoleFixReview.AgentName(tk.Title),
		StartedAt: time.Now(),
	})

	if recoveredTaskID != tk.ID {
		t.Fatalf("conflictRecovery called with task %q, want %q", recoveredTaskID, tk.ID)
	}
	got, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = %q, want unchanged (recovery handled it, not human-required)", got.Status)
	}
}

// TestOnAgentComplete_FixReviewPushDivergedEscalatesToHuman guards the other
// half: when there is no recovery path (nil callback) or it declines, the
// diverged fix must not be silently dropped — the task escalates to
// human-required rather than leaving the fix stranded with no signal.
func TestOnAgentComplete_FixReviewPushDivergedEscalatesToHuman(t *testing.T) {
	h, taskMgr, tk := setupDivergedFixReviewPush(t)
	h.conflictRecovery = func(string) bool { return false }

	h.OnComplete(&agent.Agent{
		ID:        "agent-1",
		TaskID:    tk.ID,
		Mode:      "headless",
		Name:      agent.RoleFixReview.AgentName(tk.Title),
		StartedAt: time.Now(),
	})

	got, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
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
