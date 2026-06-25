package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestPrepareForTask_AdoptsExternalWorktree verifies that a task carrying an
// explicit WorktreeDir is run in that directory verbatim: PrepareForTask
// returns the external path, records its branch, drops the identity beacon,
// creates nothing under the managed worktrees dir, and Remove leaves it alone.
func TestPrepareForTask_AdoptsExternalWorktree(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	// Simulate an Orca-created worktree: a real linked worktree off the bare
	// clone, checked out on its own branch.
	ext := filepath.Join(t.TempDir(), "orca-worktree")
	const extBranch = "orca/feature-x"
	if out, err := exec.Command("git", "-C", h.proj.ClonePath, "worktree", "add", "-b", extBranch, ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk, err := h.tasks.Create("adopt me", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id":   h.proj.ID,
		"worktree_dir": ext,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, err := h.m.PrepareForTask(tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}
	if got != ext {
		t.Fatalf("adopted path = %q, want %q", got, ext)
	}

	// Beacon written into the adopted dir.
	if _, err := os.Stat(filepath.Join(ext, contextFileName)); err != nil {
		t.Errorf("context beacon not written: %v", err)
	}

	// Branch recorded from the adopted worktree's checkout.
	reloaded, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if reloaded.Branch != extBranch {
		t.Errorf("task branch = %q, want %q", reloaded.Branch, extBranch)
	}

	// Nothing created under the managed worktrees dir.
	entries, err := os.ReadDir(h.wtDir)
	if err != nil {
		t.Fatalf("read worktrees dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("managed worktrees dir not empty: %v", entries)
	}

	// Remove must not delete an adopted worktree.
	h.m.Remove(tk.ID)
	if _, err := os.Stat(ext); err != nil {
		t.Errorf("adopted worktree removed by cleanup: %v", err)
	}
}

// TestPrepareForFix_AdoptsExternalWorktree is the regression for the
// circuit-breaker strand: a handoff/adopted worktree sent through the PR-fix
// flow must be reused as-is, not re-created with `git worktree add` (which
// fails "already exists" and, after wtFailureLimit retries, flips the task to
// human-required). The adoption guard short-circuits before any PR-branch
// fetch, so the PR number is irrelevant here.
func TestPrepareForFix_AdoptsExternalWorktree(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-worktree")
	const extBranch = "orca/fix-x"
	if out, err := exec.Command("git", "-C", h.proj.ClonePath, "worktree", "add", "-b", extBranch, ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk, err := h.tasks.Create("adopt fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id":   h.proj.ID,
		"worktree_dir": ext,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, err := h.m.PrepareForFix(tk, 1)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	if got != ext {
		t.Fatalf("adopted path = %q, want %q", got, ext)
	}

	// No managed worktree was created — proves no `git worktree add` ran.
	entries, err := os.ReadDir(h.wtDir)
	if err != nil {
		t.Fatalf("read worktrees dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("managed worktrees dir not empty: %v", entries)
	}
}

// TestPrepareForTask_AdoptRejectsMissingDir verifies adoption fails cleanly
// when the declared worktree directory does not exist.
func TestPrepareForTask_AdoptRejectsMissingDir(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	tk := task.Task{
		ID:          "deadbeef",
		Title:       "bad adopt",
		ProjectID:   h.proj.ID,
		WorktreeDir: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	if _, err := h.m.PrepareForTask(tk, nil); err == nil {
		t.Fatal("expected error for missing adopted worktree dir, got nil")
	}
}

// TestPrepareForTask_AdoptRejectsDefaultBranch guards the blocker: adopting a
// worktree sitting on the repo's default branch would push the agent's commits
// straight to origin's default branch with no PR. Adoption must refuse it.
func TestPrepareForTask_AdoptRejectsDefaultBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-on-main")
	if out, err := exec.Command("git", "-C", h.proj.ClonePath, "worktree", "add", ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk := task.Task{ID: "cafe1234", Title: "adopt main", ProjectID: h.proj.ID, WorktreeDir: ext}
	if _, err := h.m.PrepareForTask(tk, nil); err == nil {
		t.Fatal("expected error adopting a worktree on the default branch, got nil")
	}
}

// TestPrepareForTask_AdoptRejectsDetachedHead guards the empty-branch path: a
// detached-HEAD worktree has no branch to push, so adoption must refuse rather
// than silently proceed with an empty branch.
func TestPrepareForTask_AdoptRejectsDetachedHead(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-detached")
	if out, err := exec.Command("git", "-C", h.proj.ClonePath, "worktree", "add", "--detach", ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add --detach: %v: %s", err, out)
	}

	tk := task.Task{ID: "beef5678", Title: "adopt detached", ProjectID: h.proj.ID, WorktreeDir: ext}
	if _, err := h.m.PrepareForTask(tk, nil); err == nil {
		t.Fatal("expected error adopting a detached-HEAD worktree, got nil")
	}
}
