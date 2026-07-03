package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	}
	if len(out) != 0 {
		t.Errorf("worktree not clean after sync: %q", out)
	}
	for _, marker := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(wtPath, ".git", marker)); err == nil {
			t.Errorf("worktree left in mid-%s state after sync", marker)
		}
	}
}

func TestSyncTaskBranch_NoWorktreeSkipped(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("no worktree task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	}
	if result != SyncSkipped {
		t.Errorf("result = %q, want %q", result, SyncSkipped)
	}
}

func TestSyncTaskBranch_Noop(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("noop task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != SyncNoop {
		t.Errorf("result = %q, want %q", result, SyncNoop)
	}
	assertWorktreeClean(t, wtPath)
	if got, _ := h.tasks.Get(tk.ID); got.Status != task.StatusTodo {
		t.Errorf("task status = %q, want unchanged %q", got.Status, task.StatusTodo)
	}
}

func TestSyncTaskBranch_Synced(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("synced task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Upstream gains a new commit unrelated to anything the branch touched —
	// a clean fast-forward-able sync.
	if err := os.WriteFile(filepath.Join(h.src, "NEWFILE.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "NEWFILE.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "add newfile")

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != SyncSynced {
		t.Errorf("result = %q, want %q", result, SyncSynced)
	}
	assertWorktreeClean(t, wtPath)
	if _, err := os.Stat(filepath.Join(wtPath, "NEWFILE.md")); err != nil {
		t.Errorf("expected NEWFILE.md to be present after sync: %v", err)
	}
	if got, _ := h.tasks.Get(tk.ID); got.Status != task.StatusTodo {
		t.Errorf("task status = %q, want unchanged %q", got.Status, task.StatusTodo)
	}
}

func TestSyncTaskBranch_Conflict(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("conflicting task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	wtPath, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "add", "README.md")
	mustRunInDir(t, wtPath, "git", "commit", "-m", "branch edit")

	if err := os.WriteFile(filepath.Join(h.src, "README.md"), []byte("upstream edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, h.src, "git", "add", "README.md")
	mustRunInDir(t, h.src, "git", "commit", "-m", "upstream edit")

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if !errors.Is(err, ErrRebaseFailed) {
		t.Fatalf("err = %v, want ErrRebaseFailed", err)
	}
	if result != SyncConflict {
		t.Errorf("result = %q, want %q", result, SyncConflict)
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
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Point the task at a project that doesn't exist in the store — a
	// transient/config failure, not a content conflict.
	tk.ProjectID = "missing/project"

	result, err := h.m.SyncTaskBranch(context.Background(), tk)
	if err == nil {
		t.Fatal("expected error resolving missing project")
	}
	if result != SyncFailed {
		t.Errorf("result = %q, want %q", result, SyncFailed)
	}
	if errors.Is(err, ErrRebaseFailed) {
		t.Errorf("err = %v, must not classify a config/lookup failure as a rebase conflict", err)
	}
}
