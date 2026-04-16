package sybra

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// initGitRepo initialises a bare-minimum git repo in dir with a single commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	} {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v: %s", args, err, out)
		}
	}
}

// gitCommit stages everything and makes a commit.
func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", msg},
	} {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git commit %v: %v: %s", args, err, out)
		}
	}
}

// newDiffSvc returns an AgentService wired to a temp task store and a
// worktree.Manager rooted at worktreesDir.
func newDiffSvc(t *testing.T, tasksDir, worktreesDir string) (*AgentService, *task.Store) {
	t.Helper()
	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	wm := worktree.New(worktree.Config{
		WorktreesDir: worktreesDir,
		Tasks:        task.NewManager(store, nil),
		Logger:       quietLogger(),
	})
	svc := &AgentService{
		tasks:     task.NewManager(store, nil),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:       &config.Config{},
		worktrees: wm,
	}
	return svc, store
}

// taskWithWorktreeDir creates a task and the corresponding worktree directory.
func taskWithWorktreeDir(t *testing.T, store *task.Store, worktreesDir, title string) task.Task {
	t.Helper()
	tk, err := store.Create(title, "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(worktreesDir, tk.DirName())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestGetAgentDiff_NoWorktree(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	svc, store := newDiffSvc(t, filepath.Join(home, "tasks"), filepath.Join(home, "worktrees"))

	// Task exists but its worktree directory was never created.
	tk, err := store.Create("no-worktree", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	diff, err := svc.GetAgentDiff(tk.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff != "" {
		t.Errorf("want empty diff, got %q", diff)
	}
}

func TestGetAgentDiff_CleanTree(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	worktreesDir := filepath.Join(home, "worktrees")
	svc, store := newDiffSvc(t, filepath.Join(home, "tasks"), worktreesDir)

	tk := taskWithWorktreeDir(t, store, worktreesDir, "clean-tree")
	dir := filepath.Join(worktreesDir, tk.DirName())

	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir, "initial")

	diff, err := svc.GetAgentDiff(tk.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff != "" {
		t.Errorf("want empty diff for clean tree, got %q", diff)
	}
}

func TestGetAgentDiff_DirtyTree(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	worktreesDir := filepath.Join(home, "worktrees")
	svc, store := newDiffSvc(t, filepath.Join(home, "tasks"), worktreesDir)

	tk := taskWithWorktreeDir(t, store, worktreesDir, "dirty-tree")
	dir := filepath.Join(worktreesDir, tk.DirName())

	initGitRepo(t, dir)
	filePath := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(filePath, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir, "initial")

	// Modify tracked file.
	if err := os.WriteFile(filePath, []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := svc.GetAgentDiff(tk.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(diff, "-original") {
		t.Errorf("want deletion line in diff, got:\n%s", diff)
	}
	if !strings.Contains(diff, "+modified") {
		t.Errorf("want addition line in diff, got:\n%s", diff)
	}
}

func TestGetAgentDiff_UntrackedFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	worktreesDir := filepath.Join(home, "worktrees")
	svc, store := newDiffSvc(t, filepath.Join(home, "tasks"), worktreesDir)

	tk := taskWithWorktreeDir(t, store, worktreesDir, "untracked")
	dir := filepath.Join(worktreesDir, tk.DirName())

	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir, "initial")

	// New untracked file.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("newcontent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := svc.GetAgentDiff(tk.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(diff, "new.txt") {
		t.Errorf("want synthetic new-file section for new.txt, got:\n%s", diff)
	}
	if !strings.Contains(diff, "+newcontent") {
		t.Errorf("want file content in synthetic diff, got:\n%s", diff)
	}
}

func TestGetAgentDiff_NonGitDir(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	worktreesDir := filepath.Join(home, "worktrees")
	svc, store := newDiffSvc(t, filepath.Join(home, "tasks"), worktreesDir)

	tk := taskWithWorktreeDir(t, store, worktreesDir, "non-git")
	dir := filepath.Join(worktreesDir, tk.DirName())

	// Directory exists but is not a git repository.
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err := svc.GetAgentDiff(tk.ID)
	if err != nil {
		t.Fatalf("unexpected error for non-git dir: %v", err)
	}
	if diff != "" {
		t.Errorf("want empty diff for non-git dir, got %q", diff)
	}
}
