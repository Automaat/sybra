package worktree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// assertWorktreeClean fails the test if wtPath has uncommitted changes or a
// stuck merge/rebase in progress — SyncTaskBranch must leave the worktree
// clean regardless of the outcome it returns.
func assertWorktreeClean(t *testing.T, wtPath string) {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
		panic("unreachable")
	}
	if len(out) != 0 {
		t.Errorf("worktree not clean after sync: %q", out)
	}
	for _, marker := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply"} {
		gitPath := exec.Command("git", "rev-parse", "--git-path", marker)
		gitPath.Dir = wtPath
		resolved, err := gitPath.CombinedOutput()
		if err != nil {
			t.Fatalf("git rev-parse --git-path %s: %v: %s", marker, err, resolved)
			panic("unreachable")
		}
		if _, err := os.Stat(filepath.Clean(string(bytes.TrimSpace(resolved)))); err == nil {
			t.Errorf("worktree left in mid-%s state after sync", marker)
		}
	}
}

func TestSyncTaskBranch_NoWorktreeSkipped(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("no worktree task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if result != SyncSkipped {
		t.Errorf("result = %q, want %q", result, SyncSkipped)
	}
}

func TestSyncTaskBranch_AdoptedWorktreeSkipped(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk := task.Task{
		ID:          "adopted1234",
		ProjectID:   h.proj.ID,
		Status:      task.StatusTodo,
		AgentMode:   "headless",
		WorktreeDir: t.TempDir(),
	}

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if result != SyncSkipped {
		t.Errorf("result = %q, want %q", result, SyncSkipped)
	}
}

// TestSyncTaskBranch_LiveAgentSkipped proves the opportunistic branch sync
// never rebases a worktree a tracked agent is still live in — sync_branch
// is best-effort and must not risk corrupting an in-flight run.
func TestSyncTaskBranch_LiveAgentSkipped(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("live agent sync task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	mWithLiveAgent := New(Config{
		WorktreesDir: h.wtDir,
		Projects:     h.store,
		Tasks:        h.tasks,
		Logger:       discardLogger(),
		LogsDir:      h.logsDir,
		AgentChecker: func(taskID string) bool { return taskID == tk.ID },
	})

	result, err := mWithLiveAgent.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if result != SyncSkipped {
		t.Errorf("result = %q, want %q", result, SyncSkipped)
	}
	assertWorktreeClean(t, wtPath)
}

func TestSyncTaskBranch_Noop(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("noop task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if result != SyncNoop {
		t.Errorf("result = %q, want %q", result, SyncNoop)
	}
	assertWorktreeClean(t, wtPath)
	if got, _ := h.tasks.Get(tk.ID); got.Status != task.StatusTodo {
		t.Errorf("task status = %q, want unchanged %q", got.Status, task.StatusTodo)
	}
}

func TestSyncTaskBranch_PushedBranchSkipsBaseMerge(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("synced task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	preHEAD := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "rev-parse", "HEAD"))

	// Upstream gains a new commit unrelated to anything the branch touched.
	// The task branch was already pushed by PrepareForTask, so SyncTaskBranch
	// must not merge base just to refresh it.
	if err := os.WriteFile(filepath.Join(h.src, "NEWFILE.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	mustRunInDir(t, h.src, "git", "add", "NEWFILE.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "add newfile")

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if result != SyncNoop {
		t.Errorf("result = %q, want %q", result, SyncNoop)
	}
	assertWorktreeClean(t, wtPath)
	if got := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "rev-parse", "HEAD")); got != preHEAD {
		t.Fatalf("HEAD moved after non-conflict base advance: got %s want %s", got, preHEAD)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "NEWFILE.md")); !os.IsNotExist(err) {
		t.Errorf("NEWFILE.md should not be merged into pushed task branch, stat err=%v", err)
	}
	if got, _ := h.tasks.Get(tk.ID); got.Status != task.StatusTodo {
		t.Errorf("task status = %q, want unchanged %q", got.Status, task.StatusTodo)
	}
}

func TestSyncTaskBranch_PushedBranchSkipsBaseConflict(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("conflicting task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "branch edit")

	if err := os.WriteFile(filepath.Join(h.src, "README.md"), []byte("upstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	mustRunInDir(t, h.src, "git", "add", "README.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream edit")

	preHEAD := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "rev-parse", "HEAD"))
	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if result != SyncNoop {
		t.Errorf("result = %q, want %q", result, SyncNoop)
	}
	assertWorktreeClean(t, wtPath)
	if got := strings.TrimSpace(mustOutputInDir(t, wtPath, "git", "rev-parse", "HEAD")); got != preHEAD {
		t.Fatalf("HEAD moved after conflicting base advance: got %s want %s", got, preHEAD)
	}
	if got, _ := h.tasks.Get(tk.ID); got.Status != task.StatusTodo {
		t.Errorf("task status = %q, want unchanged (sync never escalates status)", got.Status)
	}
}

func TestSyncTaskBranch_FailedGetProject(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("orphan task", "", "headless")
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	// Point the task at a project that doesn't exist in the store — a
	// transient/config failure, not a content conflict.
	tk.ProjectID = "missing/project"

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err == nil {
		t.Fatal("expected error resolving missing project")
		panic("unreachable")
	}
	if result != SyncFailed {
		t.Errorf("result = %q, want %q", result, SyncFailed)
	}
	if errors.Is(err, ErrRebaseFailed) {
		t.Errorf("err = %v, must not classify a config/lookup failure as a rebase conflict", err)
	}
}
