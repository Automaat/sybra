package completion

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestOnAgentComplete_EmptyTaskID_NoCrash(t *testing.T) {
	// Orchestrator brain agents run with TaskID="" — feeding that into
	// UpdateRun/HandleAgentComplete/Get used to crash the handler with
	// "open .sybra/tasks/.md: no such file or directory" because the
	// empty ID was joined to the tasks dir. Verify the short-circuit.
	dir, err := os.MkdirTemp("", "sybra-test-tasks-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	wm := worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: tasks, Logger: logger})

	// Pre-existing task that must NOT be touched: an empty-id call must
	// not accidentally rewrite a task file (the original bug joined "" to
	// the tasks dir as ".md" — the path it would have hit is here too).
	other, err := tasks.Create("Other task", "body", "headless")
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

	h := New(Config{Logger: logger, Tasks: tasks, Worktrees: wm})

	// Should not panic, and should not touch any task file. The historical
	// bug created/touched ".md" in tasksDir; assert no such file exists.
	h.OnComplete(ag)

	if _, err := os.Stat(filepath.Join(dir, ".md")); !os.IsNotExist(err) {
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

func setupFixReviewPushTest(t *testing.T) (h *Handler, taskMgr *task.Manager, barePath, src string) {
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

	h = New(Config{Logger: logger, Tasks: taskMgr, Worktrees: wm})
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
func setupDivergedFixReviewPush(t *testing.T) (h *Handler, taskMgr *task.Manager, tk task.Task) {
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
