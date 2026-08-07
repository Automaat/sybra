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
	"time"

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
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("SYBRA_HOME", home)

	tasksDir := filepath.Join(home, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	taskMgr := task.NewManager(taskStore, nil)

	projStore, err := project.NewStore(
		filepath.Join(home, "projects"),
		filepath.Join(home, "clones"),
	)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	// Build a real source repo with a commit, then clone it as bare so
	// worktree operations (git worktree add) have a valid refs/remotes/origin.
	src := initSourceRepo(t)
	barePath := filepath.Join(home, "clones", "testowner", "testrepo.git")
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := project.CloneBare(context.Background(), src, barePath); err != nil {
		t.Fatalf("clone bare: %v", err)
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
			panic("unreachable")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	commit := [][]string{
		{"-C", dir, "add", "."},
		{"-C", dir, "commit", "-m", "init"},
	}
	for _, args := range commit {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
			panic("unreachable")
		}
	}
	return dir
}

func TestReviewService_StartFixReview_NotFound(t *testing.T) {
	svc, _, _ := setupReviewService(t)
	if err := svc.StartFixReview("does-not-exist"); err == nil {
		t.Fatal("expected error for nonexistent task")
		panic("unreachable")
	}
}

func TestReviewService_StartFixReview_NoPR(t *testing.T) {
	svc, taskMgr, _ := setupReviewService(t)

	tk, err := taskMgr.Create("no pr task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	err = svc.StartFixReview(tk.ID)
	if err == nil {
		t.Fatal("expected error for task with no linked PR")
		panic("unreachable")
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
		panic("unreachable")
	}
	if _, err := taskMgr.Update(tk.ID, task.Update{PRNumber: task.Ptr(42)}); err != nil {
		t.Fatal(err)
	}

	err = svc.StartFixReview(tk.ID)
	if err == nil {
		t.Fatal("expected error for task without projectID")
		panic("unreachable")
	}
}

func TestReviewService_StartFixReview_HappyPath(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "claude-args.log")
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", argsLog)
	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.log")
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", stdinLog)

	svc, taskMgr, _ := setupReviewService(t)

	tk, err := taskMgr.Create("fix pr 42", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := taskMgr.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("testowner/testrepo"),
		PRNumber:  task.Ptr(42),
		Status:    task.Ptr(task.StatusInReview),
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.StartFixReview(tk.ID); err != nil {
		t.Fatalf("StartFixReview: %v", err)
		panic("unreachable")
	}

	wtPath := filepath.Join(os.Getenv("SYBRA_HOME"), "worktrees", tk.DirName())
	branchOut, err := exec.Command("git", "-C", wtPath, "branch", "--show-current").Output()
	if err != nil {
		t.Fatalf("show current branch: %v", err)
		panic("unreachable")
	}
	if branch := strings.TrimSpace(string(branchOut)); branch == "" {
		t.Fatalf("fix-review worktree is detached HEAD")
	}

	waitFor(t, 5*time.Second, "fake-claude args log written", func() bool {
		_, err := os.Stat(argsLog)
		return err == nil
	})

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	args := string(data)

	for _, want := range []string{
		"-p",
		"--output-format",
		"stream-json",
		"--model",
		"opus",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in args:\n%s", want, args)
		}
	}

	// The prompt itself travels over stdin, not argv (see
	// provider_claude.go's non-steerable branch) — check the captured
	// stdin content, not args, for its actual text.
	waitFor(t, 5*time.Second, "fake-claude stdin log written", func() bool {
		_, err := os.Stat(stdinLog)
		return err == nil
	})
	stdinData, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	prompt := string(stdinData)
	for _, want := range []string{
		"/fix-review https://github.com/testowner/testrepo/pull/42 --auto",
		"fix(review)",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected %q in stdin prompt:\n%s", want, prompt)
		}
	}

	// Verify the agent got registered on the task with the fix-review role.
	waitFor(t, 2*time.Second, "fix-review agent run recorded", func() bool {
		cur, err := taskMgr.Get(tk.ID)
		if err != nil {
			return false
		}
		for _, run := range cur.AgentRuns {
			if run.Role == string(agent.RoleFixReview) {
				return true
			}
		}
		return false
	})
}
