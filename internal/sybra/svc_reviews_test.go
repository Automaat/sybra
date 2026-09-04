//go:build e2e

package sybra

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/review"
	"github.com/Automaat/sybra/internal/task"

	"github.com/Automaat/sybra/internal/worktree"
)

// setupReviewService wires a ReviewService + review.Handler backed by real
// task/project/worktree stores and the fake-claude binary for agent runs.
// Returns the service, task manager, and the bare clone path the test can
// use to simulate PR branches.
func setupReviewService(t *testing.T) (*ReviewService, *task.Manager, string) {
	t.Helper()
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "success")

	home, err := os.MkdirTemp("", "sybra-rev-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("SYBRA_HOME", home)

	tasksDir := filepath.Join(home, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	projStore, err := project.NewStore(
		filepath.Join(home, "projects"),
		filepath.Join(home, "clones"),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Build a real source repo with a commit, then clone it as bare so
	// worktree operations (git worktree add) have a valid refs/remotes/origin.
	src := initSourceRepo(t)
	barePath := filepath.Join(home, "clones", "testowner", "testrepo.git")
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.CloneBare(context.Background(), src, barePath); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	// Seed a project YAML manually — project.Store.Create would try to
	// clone from a live URL which we don't have in tests.
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

	logger := slog.New(slog.DiscardHandler)
	logDir := filepath.Join(home, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, logDir, agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{DefaultProvider: "claude"},
	})

	wm := worktree.New(worktree.Config{
		WorktreesDir: filepath.Join(home, "worktrees"),
		Projects:     projStore,
		Tasks:        taskMgr,
		Logger:       logger,
		PRBranchResolver: func(repo string, prNumber int) (string, error) {
			// Resolve to the default branch so CreateWorktreeExisting finds
			// refs/remotes/origin/<branch>.
			return project.DefaultBranch(context.Background(), barePath)
		},
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})

	handler := review.New(taskMgr, projStore, agentMgr, nil, logger, nil, func(string, any) {}, wm, nil, nil, nil)
	svc := &ReviewService{reviewer: handler, tasks: taskMgr}

	return svc, taskMgr, barePath
}

func initSourceRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runs := [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@test.com"},
		{"-C", dir, "config", "user.name", "Test"},
		{"-C", dir, "config", "commit.gpgsign", "false"},
	}
	for _, args := range runs {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := [][]string{
		{"-C", dir, "add", "."},
		{"-C", dir, "commit", "-m", "init"},
	}
	for _, args := range commit {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestReviewService_StartFixReview_NotFound(t *testing.T) {
	svc, _, _ := setupReviewService(t)
	if err := svc.StartFixReview("does-not-exist"); err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestReviewService_StartFixReview_NoPR(t *testing.T) {
	svc, taskMgr, _ := setupReviewService(t)

	tk, err := taskMgr.Create("no pr task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	err = svc.StartFixReview(tk.ID)
	if err == nil {
		t.Fatal("expected error for task with no linked PR")
	}
	if !strings.Contains(err.Error(), "no linked PR") {
		t.Errorf("error = %q, want substring %q", err.Error(), "no linked PR")
	}
}

func TestReviewService_StartFixReview_NoProjectID(t *testing.T) {
	svc, taskMgr, _ := setupReviewService(t)

	tk, err := taskMgr.Create("pr without project", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskMgr.Update(tk.ID, task.Update{PRNumber: task.Ptr(42)}); err != nil {
		t.Fatal(err)
	}

	err = svc.StartFixReview(tk.ID)
	if err == nil {
		t.Fatal("expected error for task without projectID")
	}
}
