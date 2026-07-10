package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/notes"
	"github.com/Automaat/sybra/internal/task"
)

// TestPrepareForBranchFix_ExistingBranch verifies the happy path: a task
// whose branch already exists (locally or on origin) gets a worktree checked
// out on that branch by name — no PR lookup involved.
func TestPrepareForBranchFix_ExistingBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const branch = "fix/no-pr-branch"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "branch commit")
	srcGit("checkout", "main")

	tk, err := h.tasks.Create("no pr branch fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"branch":     branch,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForBranchFix(context.Background(), tk)
	if err != nil {
		t.Fatalf("PrepareForBranchFix: %v", err)
	}

	headBranch, err := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD branch: %v: %s", err, headBranch)
	}
	if got := strings.TrimSpace(string(headBranch)); got != branch {
		t.Errorf("worktree branch = %q, want %q", got, branch)
	}

	reloaded, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if reloaded.Branch != branch {
		t.Errorf("task branch = %q, want %q", reloaded.Branch, branch)
	}
}

// TestPrepareForBranchFix_ReusesHealthyWorktree mirrors
// TestPrepareForFix_ReusesHealthyWorktree: a second dispatch against a
// healthy worktree already on the task's branch must reuse it (no setup
// re-run), while still fast-forwarding to any commit pushed in the meantime.
func TestPrepareForBranchFix_ReusesHealthyWorktree(t *testing.T) {
	h := prepareHarness(t, []string{"sh -c 'echo run >> setup-count.txt'"}, 0)

	const branch = "fix/branch-reuse-me"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// setup-count.txt is a byproduct of the setup command, not agent work —
	// gitignore it so SanitizeWorktree's auto-commit-uncommitted-work step
	// doesn't turn it into a local-only commit that diverges from the next
	// remote push and defeats reuse.
	if err := os.WriteFile(filepath.Join(h.src, ".gitignore"), []byte("setup-count.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "gitignore setup byproduct")
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "initial branch commit")
	srcGit("checkout", "main")

	tk, err := h.tasks.Create("reuse branch fix worktree", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"branch":     branch,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForBranchFix(context.Background(), tk)
	if err != nil {
		t.Fatalf("PrepareForBranchFix (initial): %v", err)
	}
	setupCountPath := filepath.Join(wtPath, "setup-count.txt")
	countAfterFirst, err := os.ReadFile(setupCountPath)
	if err != nil {
		t.Fatalf("read setup-count.txt: %v", err)
	}
	if got := strings.Count(string(countAfterFirst), "run"); got != 1 {
		t.Fatalf("setup ran %d times on initial prepare, want 1", got)
	}
	hookPath := signoffHookPath(t, wtPath)
	if err := os.Remove(hookPath); err != nil {
		t.Fatalf("remove signoff hook: %v", err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "config", "--unset", "core.hooksPath").CombinedOutput(); err != nil {
		t.Fatalf("unset core.hooksPath: %v: %s", err, out)
	}

	srcGit("checkout", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature2.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "second branch commit")
	srcGit("checkout", "main")

	got, err := h.m.PrepareForBranchFix(context.Background(), tk)
	if err != nil {
		t.Fatalf("PrepareForBranchFix (reuse): %v", err)
	}
	if got != wtPath {
		t.Fatalf("reused path = %q, want the same worktree %q", got, wtPath)
	}

	countAfterSecond, err := os.ReadFile(setupCountPath)
	if err != nil {
		t.Fatalf("read setup-count.txt after reuse: %v", err)
	}
	if got := strings.Count(string(countAfterSecond), "run"); got != 1 {
		t.Fatalf("setup ran %d times across both prepares, want 1 (setup must be skipped on reuse)", got)
	}

	if _, err := os.Stat(filepath.Join(got, "feature2.txt")); err != nil {
		t.Errorf("reused worktree missing feature2.txt from the second remote commit: %v", err)
	}
	if _, err := os.Stat(hookPath); err != nil {
		t.Errorf("signoff hook was not restored on reuse: %v", err)
	}
}

// TestPrepareForBranchFix_SetupFailureDoesNotBlock is the regression test for
// issue #1454: a project whose setup: command fails (e.g. a broken build)
// must not prevent PrepareForBranchFix from creating the fix worktree — that
// would deadlock the task, since the fixer this worktree is for exists
// specifically to repair the breakage.
func TestPrepareForBranchFix_SetupFailureDoesNotBlock(t *testing.T) {
	h := prepareHarness(t, []string{"exit 1"}, 0)

	const branch = "fix/broken-setup-branch"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "branch commit")
	srcGit("checkout", "main")

	tk, err := h.tasks.Create("broken setup branch fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"branch":     branch,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForBranchFix(context.Background(), tk)
	if err != nil {
		t.Fatalf("PrepareForBranchFix must succeed despite a failing setup command: %v", err)
	}

	content, readErr := os.ReadFile(filepath.Join(wtPath, notes.FileName))
	if readErr != nil {
		t.Fatalf("read scratchpad: %v", readErr)
	}
	if !strings.Contains(string(content), "Setup failure") {
		t.Errorf("scratchpad missing setup failure note: %q", content)
	}
}

// TestPrepareForBranchFix_MissingBranch verifies the hard-failure path: a
// task branch that exists neither locally nor on origin returns
// ErrTaskBranchMissing so the caller can escalate rather than guess at a ref
// (unlike PrepareForFix, there is no PR head to fall back to here).
func TestPrepareForBranchFix_MissingBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	tk, err := h.tasks.Create("missing branch fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"branch":     "fix/does-not-exist-anywhere",
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	_, err = h.m.PrepareForBranchFix(context.Background(), tk)
	if !errors.Is(err, ErrTaskBranchMissing) {
		t.Fatalf("PrepareForBranchFix error = %v, want ErrTaskBranchMissing", err)
	}
}

// TestPrepareForBranchFix_FetchFailureDoesNotMasqueradeAsMissingBranch verifies
// that a transient fetch failure is surfaced as such when the task branch is
// only discoverable via origin, rather than being misclassified as
// ErrTaskBranchMissing.
func TestPrepareForBranchFix_FetchFailureDoesNotMasqueradeAsMissingBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const branch = "fix/fetch-needed"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "fetch-needed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "fetch-needed branch")
	srcGit("checkout", "main")

	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing-origin.git")).CombinedOutput(); err != nil {
		t.Fatalf("git remote set-url: %v: %s", err, out)
	}

	tk, err := h.tasks.Create("fetch failure branch fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"branch":     branch,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	_, err = h.m.PrepareForBranchFix(context.Background(), tk)
	if err == nil {
		t.Fatal("PrepareForBranchFix error = nil, want fetch failure")
	}
	if errors.Is(err, ErrTaskBranchMissing) {
		t.Fatalf("PrepareForBranchFix error = %v, must not be ErrTaskBranchMissing when fetch failed", err)
	}
}

// TestPrepareForBranchFix_AdoptsExternalWorktree mirrors
// TestPrepareForFix_AdoptsExternalWorktree: a task carrying an explicit
// WorktreeDir must be reused as-is, never re-created with `git worktree add`.
func TestPrepareForBranchFix_AdoptsExternalWorktree(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-worktree")
	const extBranch = "orca/branch-fix-x"
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "worktree", "add", "-b", extBranch, ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk, err := h.tasks.Create("adopt branch fix", "", task.AgentModeHeadless)
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

	got, err := h.m.PrepareForBranchFix(context.Background(), tk)
	if err != nil {
		t.Fatalf("PrepareForBranchFix: %v", err)
	}
	if got != ext {
		t.Fatalf("adopted path = %q, want %q", got, ext)
	}

	entries, err := os.ReadDir(h.wtDir)
	if err != nil {
		t.Fatalf("read worktrees dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("managed worktrees dir not empty: %v", entries)
	}
}
