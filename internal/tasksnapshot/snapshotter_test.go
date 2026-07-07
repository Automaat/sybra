package tasksnapshot

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newTestSnapshotter(t *testing.T) (snap *Snapshotter, gitDir, workTree string) {
	t.Helper()
	root := t.TempDir()
	gitDir = filepath.Join(root, "snapshots.git")
	workTree = filepath.Join(root, "tasks")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatalf("mkdir workTree: %v", err)
	}
	return New(gitDir, workTree, 50*time.Millisecond, testLogger()), gitDir, workTree
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func gitLogCount(t *testing.T, gitDir, workTree string) int {
	t.Helper()
	cmd := exec.Command("git", "log", "--oneline")
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+workTree)
	out, err := cmd.Output()
	if err != nil {
		// No commits yet is reported as an error by `git log`.
		return 0
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func TestEnsureRepo_InitializesFreshRepo(t *testing.T) {
	s, gitDir, _ := newTestSnapshotter(t)
	ctx := context.Background()

	if !s.EnsureRepo(ctx) {
		t.Fatal("EnsureRepo returned false for a fresh repo")
	}
	if _, err := os.Stat(gitDir); err != nil {
		t.Fatalf("expected git dir to exist: %v", err)
	}
	excludeData, err := os.ReadFile(filepath.Join(gitDir, "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	if !strings.Contains(string(excludeData), "*.lock") {
		t.Fatalf("expected *.lock in exclude file, got %q", excludeData)
	}
}

func TestEnsureRepo_IdempotentOnExistingRepo(t *testing.T) {
	s, _, _ := newTestSnapshotter(t)
	ctx := context.Background()

	if !s.EnsureRepo(ctx) {
		t.Fatal("first EnsureRepo failed")
	}
	if !s.EnsureRepo(ctx) {
		t.Fatal("second EnsureRepo failed on already-initialized repo")
	}
}

func TestEnsureRepo_ReusesRepoAcrossInstances(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "snapshots.git")
	workTree := filepath.Join(root, "tasks")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatalf("mkdir workTree: %v", err)
	}
	ctx := context.Background()

	s1 := New(gitDir, workTree, time.Second, testLogger())
	if !s1.EnsureRepo(ctx) {
		t.Fatal("first instance EnsureRepo failed")
	}
	writeFile(t, filepath.Join(workTree, "a.md"), "a")
	if committed, err := s1.Commit(ctx); err != nil || !committed {
		t.Fatalf("first commit: committed=%v err=%v", committed, err)
	}

	s2 := New(gitDir, workTree, time.Second, testLogger())
	if !s2.EnsureRepo(ctx) {
		t.Fatal("second instance failed to reuse existing repo")
	}
	if got := gitLogCount(t, gitDir, workTree); got != 1 {
		t.Fatalf("expected 1 commit preserved across instances, got %d", got)
	}
}

func TestCommit_OnChange(t *testing.T) {
	s, gitDir, workTree := newTestSnapshotter(t)
	ctx := context.Background()
	if !s.EnsureRepo(ctx) {
		t.Fatal("EnsureRepo failed")
	}

	writeFile(t, filepath.Join(workTree, "task-1.md"), "hello")

	committed, err := s.Commit(ctx)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !committed {
		t.Fatal("expected a commit for a new file")
	}
	if got := gitLogCount(t, gitDir, workTree); got != 1 {
		t.Fatalf("expected 1 commit, got %d", got)
	}
	if s.LastCommit().IsZero() {
		t.Fatal("expected LastCommit to be set after a successful commit")
	}
}

func TestCommit_NoOpWhenClean(t *testing.T) {
	s, gitDir, workTree := newTestSnapshotter(t)
	ctx := context.Background()
	if !s.EnsureRepo(ctx) {
		t.Fatal("EnsureRepo failed")
	}

	writeFile(t, filepath.Join(workTree, "task-1.md"), "hello")
	if committed, err := s.Commit(ctx); err != nil || !committed {
		t.Fatalf("first commit: committed=%v err=%v", committed, err)
	}

	committed, err := s.Commit(ctx)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if committed {
		t.Fatal("expected no-op commit on a clean tree")
	}
	if got := gitLogCount(t, gitDir, workTree); got != 1 {
		t.Fatalf("expected commit count unchanged at 1, got %d", got)
	}
}

func TestCommit_CapturesExternalDelete(t *testing.T) {
	s, gitDir, workTree := newTestSnapshotter(t)
	ctx := context.Background()
	if !s.EnsureRepo(ctx) {
		t.Fatal("EnsureRepo failed")
	}

	taskPath := filepath.Join(workTree, "task-1.md")
	writeFile(t, taskPath, "hello")
	if committed, err := s.Commit(ctx); err != nil || !committed {
		t.Fatalf("baseline commit: committed=%v err=%v", committed, err)
	}

	// Simulate a raw `rm` that bypasses the store entirely.
	if err := os.Remove(taskPath); err != nil {
		t.Fatalf("remove task file: %v", err)
	}

	committed, err := s.Commit(ctx)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !committed {
		t.Fatal("expected the external delete to be captured as a commit")
	}
	if got := gitLogCount(t, gitDir, workTree); got != 2 {
		t.Fatalf("expected 2 commits, got %d", got)
	}
}

func TestCommit_RmThenRestoreRoundTrip(t *testing.T) {
	s, gitDir, workTree := newTestSnapshotter(t)
	ctx := context.Background()
	if !s.EnsureRepo(ctx) {
		t.Fatal("EnsureRepo failed")
	}

	taskPath := filepath.Join(workTree, "task-1.md")
	writeFile(t, taskPath, "original content")
	if committed, err := s.Commit(ctx); err != nil || !committed {
		t.Fatalf("baseline commit: committed=%v err=%v", committed, err)
	}

	if err := os.Remove(taskPath); err != nil {
		t.Fatalf("remove task file: %v", err)
	}
	if committed, err := s.Commit(ctx); err != nil || !committed {
		t.Fatalf("delete commit: committed=%v err=%v", committed, err)
	}
	if _, err := os.Stat(taskPath); !os.IsNotExist(err) {
		t.Fatalf("expected task file to be gone before restore, err=%v", err)
	}

	// Recovery path: git checkout the pre-delete commit against this
	// work-tree, exactly as documented in docs/tasks-snapshots.md.
	cmd := exec.Command("git", "checkout", "HEAD~1", "--", ".")
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+workTree)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout restore: %v: %s", err, out)
	}

	restored, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(restored) != "original content" {
		t.Fatalf("restored content = %q, want %q", restored, "original content")
	}
}

func TestCommitNow_SwallowsErrors(t *testing.T) {
	s, _, _ := newTestSnapshotter(t)
	ctx := context.Background()
	// EnsureRepo never called: git-dir does not exist. CommitNow must not
	// panic or block the caller — errors are logged and swallowed.
	s.CommitNow(ctx)
}

func TestCommitNow_NilReceiverIsSafe(t *testing.T) {
	var s *Snapshotter
	s.CommitNow(context.Background())
}

func TestEnsureRepo_DisablesOnWrongWorktree(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "snapshots.git")
	workTreeA := filepath.Join(root, "tasks-a")
	workTreeB := filepath.Join(root, "tasks-b")
	for _, d := range []string{workTreeA, workTreeB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	ctx := context.Background()

	sa := New(gitDir, workTreeA, time.Second, testLogger())
	if !sa.EnsureRepo(ctx) {
		t.Fatal("initial EnsureRepo against workTreeA failed")
	}

	sb := New(gitDir, workTreeB, time.Second, testLogger())
	if sb.EnsureRepo(ctx) {
		t.Fatal("expected EnsureRepo to disable snapshotting for a mismatched work-tree")
	}

	writeFile(t, filepath.Join(workTreeB, "task-1.md"), "hello")
	committed, err := sb.Commit(ctx)
	if err != nil {
		t.Fatalf("Commit on a disabled snapshotter should not error, got %v", err)
	}
	if committed {
		t.Fatal("expected disabled snapshotter to never commit")
	}
	if got := gitLogCount(t, gitDir, workTreeA); got != 0 {
		t.Fatalf("expected no commits leaked into the shared git dir, got %d", got)
	}
}

func TestEnsureRepo_DisablesOnMalformedRepo(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "snapshots.git")
	workTree := filepath.Join(root, "tasks")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatalf("mkdir workTree: %v", err)
	}
	// A directory that exists but is not a git repo at all.
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir gitDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "not-a-repo"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	s := New(gitDir, workTree, time.Second, testLogger())
	if s.EnsureRepo(context.Background()) {
		t.Fatal("expected EnsureRepo to disable snapshotting for a malformed repo")
	}
}

func TestCommit_TimesOutOnCanceledContext(t *testing.T) {
	s, _, _ := newTestSnapshotter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !s.EnsureRepo(context.Background()) {
		t.Fatal("EnsureRepo failed")
	}

	done := make(chan struct{})
	go func() {
		_, _ = s.Commit(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Commit did not return promptly for an already-canceled context")
	}
}

func TestRun_CommitsOnInterval(t *testing.T) {
	s, _, workTree := newTestSnapshotter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !s.EnsureRepo(context.Background()) {
		t.Fatal("EnsureRepo failed")
	}

	writeFile(t, filepath.Join(workTree, "task-1.md"), "hello")

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for s.LastCommit().IsZero() {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("Run did not produce a commit within the deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
