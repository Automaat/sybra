package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// commitFile writes and commits a single file in wtPath, returning nothing —
// callers assert file presence/absence instead of inspecting the SHA.
func commitFile(t *testing.T, wtPath, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wtPath, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunInDir(t, wtPath, "git", "config", "user.email", "test@test.com")
	mustRunInDir(t, wtPath, "git", "config", "user.name", "Test")
	mustRunInDir(t, wtPath, "git", "add", name)
	mustRunInDir(t, wtPath, "git", "commit", "-m", msg)
}

// TestPrepareAttempt_IsolatedFromCanonicalAndSiblings proves each attempt
// gets its own worktree dir and branch, distinct from the task's canonical
// worktree and from every other attempt — the core isolation property that
// distinguishes best-of-N attempts from `parallel` children (which share one
// checkout).
func TestPrepareAttempt_IsolatedFromCanonicalAndSiblings(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "isolation task")

	dir1, branch1, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_1): %v", err)
	}
	dir2, branch2, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_2")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_2): %v", err)
	}

	if dir1 == dir2 {
		t.Fatalf("attempt dirs collide: %q", dir1)
	}
	if branch1 == branch2 {
		t.Fatalf("attempt branches collide: %q", branch1)
	}
	if dir1 == h.m.PathFor(tk) || dir2 == h.m.PathFor(tk) {
		t.Fatalf("attempt dir must not equal canonical worktree dir %q (dir1=%q dir2=%q)", h.m.PathFor(tk), dir1, dir2)
	}
	for _, d := range []string{dir1, dir2} {
		if _, statErr := os.Stat(d); statErr != nil {
			t.Fatalf("attempt dir %q not created: %v", d, statErr)
		}
	}

	// Each attempt must NOT have written task.Branch — that's PromoteAttempt's
	// job, once a winner is chosen.
	if tk.Branch != "" {
		t.Errorf("PrepareAttempt must never write task.Branch; got %q", tk.Branch)
	}
}

// TestPrepareAttempt_NeverPushes confirms an attempt branch never reaches the
// project's true upstream remote (h.src, the fake GitHub in this harness) —
// attempts are local-only (to the bare clone Sybra uses as worktree storage)
// until PromoteAttempt fast-forwards the canonical branch onto a winner and
// a later push step pushes the canonical branch upstream.
func TestPrepareAttempt_NeverPushes(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "no-push task")

	dir, branch, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	commitFile(t, dir, "attempt.txt", "attempt work\n", "attempt commit")

	out, err := exec.Command("git", "-C", h.src, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("attempt branch %q must not exist on the true upstream remote before promotion", branch)
	}
}

// TestPromoteAttempt_OnlyWinnerCommitsLandOnCanonical is the core best-of-N
// promotion guarantee: after promoting attempt_2, the canonical branch/
// worktree contains attempt_2's file and NOT attempt_1's — losing attempts
// never leak onto the branch a PR gets opened from.
func TestPromoteAttempt_OnlyWinnerCommitsLandOnCanonical(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "promote task")

	dir1, _, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_1): %v", err)
	}
	commitFile(t, dir1, "loser.txt", "loser content\n", "loser commit")

	dir2, branch2, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_2")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_2): %v", err)
	}
	commitFile(t, dir2, "winner.txt", "winner content\n", "winner commit")

	canonicalDir, err := h.m.PromoteAttempt(context.Background(), tk, dir2, branch2)
	if err != nil {
		t.Fatalf("PromoteAttempt: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(canonicalDir, "winner.txt")); statErr != nil {
		t.Errorf("winner.txt missing from canonical worktree: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(canonicalDir, "loser.txt")); statErr == nil {
		t.Errorf("loser.txt present in canonical worktree — losing attempt leaked onto canonical branch")
	}

	// Also check history on the bare canonical branch, not just the working
	// tree, so a stray merge that carried the loser's blob without checking it
	// out wouldn't slip past a working-tree-only assertion.
	out, err := exec.Command("git", "-C", canonicalDir, "log", "--name-only", "--format=").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.Contains(string(out), "loser.txt") {
		t.Errorf("loser.txt appears in canonical branch history:\n%s", out)
	}
	if !strings.Contains(string(out), "winner.txt") {
		t.Errorf("winner.txt missing from canonical branch history:\n%s", out)
	}
}

// TestPromoteAttempt_Idempotent proves a re-run against a canonical branch
// already at the winner's HEAD is a safe no-op — required so a crash between
// SetBranchTo and returning from PromoteAttempt can be retried without
// side effects.
func TestPromoteAttempt_Idempotent(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "idempotent task")

	dir, branch, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	commitFile(t, dir, "winner.txt", "content\n", "winner commit")

	first, err := h.m.PromoteAttempt(context.Background(), tk, dir, branch)
	if err != nil {
		t.Fatalf("first PromoteAttempt: %v", err)
	}
	second, err := h.m.PromoteAttempt(context.Background(), tk, dir, branch)
	if err != nil {
		t.Fatalf("second (idempotent) PromoteAttempt: %v", err)
	}
	if first != second {
		t.Errorf("canonical dir changed across idempotent re-promotion: %q vs %q", first, second)
	}
	if _, statErr := os.Stat(filepath.Join(second, "winner.txt")); statErr != nil {
		t.Errorf("winner.txt missing after idempotent re-promotion: %v", statErr)
	}
}

// TestPromoteAttempt_RefusesExistingPR fails closed rather than moving a
// branch a reviewer/CI is already looking at.
func TestPromoteAttempt_RefusesExistingPR(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "has-pr task")
	tk.PRNumber = 42

	dir, branch, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	commitFile(t, dir, "winner.txt", "content\n", "winner commit")

	_, err = h.m.PromoteAttempt(context.Background(), tk, dir, branch)
	if !errors.Is(err, ErrPromotionHasPR) {
		t.Fatalf("PromoteAttempt error = %v, want ErrPromotionHasPR", err)
	}
}

// TestPromoteAttempt_RefusesDirtyWinner fails closed rather than silently
// dropping uncommitted work sitting in the winner's worktree.
func TestPromoteAttempt_RefusesDirtyWinner(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "dirty task")

	dir, branch, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	commitFile(t, dir, "winner.txt", "content\n", "winner commit")
	if err := os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = h.m.PromoteAttempt(context.Background(), tk, dir, branch)
	if !errors.Is(err, ErrPromotionDirty) {
		t.Fatalf("PromoteAttempt error = %v, want ErrPromotionDirty", err)
	}
}

// TestPromoteAttempt_RefusesDivergedCanonicalBranch fails closed rather than
// force-moving a canonical branch that already carries commits the winning
// attempt never saw (e.g. a prior non-best-of-N run left work there).
func TestPromoteAttempt_RefusesDivergedCanonicalBranch(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "diverged task")

	// Establish a canonical branch (independent of any attempt) with its own
	// commit, simulating pre-existing work on the task's branch.
	canonicalDir, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}
	commitFile(t, canonicalDir, "preexisting.txt", "already here\n", "pre-existing canonical work")

	dir, branch, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	commitFile(t, dir, "winner.txt", "content\n", "winner commit")

	_, err = h.m.PromoteAttempt(context.Background(), tk, dir, branch)
	if !errors.Is(err, ErrPromotionDiverged) {
		t.Fatalf("PromoteAttempt error = %v, want ErrPromotionDiverged", err)
	}
}

// TestCleanupAttempts_RemovesDirsFromDisk proves losing (or all-failed)
// attempt worktree directories are actually removed from disk, not just
// dropped from in-memory bookkeeping.
func TestCleanupAttempts_RemovesDirsFromDisk(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "cleanup task")

	dir1, _, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_1): %v", err)
	}
	dir2, _, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_2")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_2): %v", err)
	}

	h.m.CleanupAttempts(context.Background(), tk, []string{"attempt_1", "attempt_2"})

	for _, d := range []string{dir1, dir2} {
		if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
			t.Errorf("attempt dir %q still exists after CleanupAttempts (stat err=%v)", d, statErr)
		}
	}
}

// TestCleanupAttempts_LeavesWinnerUntouched confirms cleanup only removes
// the attempt dirs it's told to — promotion's caller passes just the losers.
func TestCleanupAttempts_LeavesWinnerUntouched(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "selective cleanup task")

	loserDir, _, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_1): %v", err)
	}
	winnerDir, _, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_2")
	if err != nil {
		t.Fatalf("PrepareAttempt(attempt_2): %v", err)
	}

	h.m.CleanupAttempts(context.Background(), tk, []string{"attempt_1"})

	if _, statErr := os.Stat(loserDir); !os.IsNotExist(statErr) {
		t.Errorf("loser dir %q still exists after cleanup", loserDir)
	}
	if _, statErr := os.Stat(winnerDir); statErr != nil {
		t.Errorf("winner dir %q was removed by cleanup targeting only the loser: %v", winnerDir, statErr)
	}
}

// TestCleanupAttempts_DeletesBranchRef proves cleanup removes the attempt's
// local branch ref, not just its worktree dir — otherwise a later best-of-N
// cycle for the same task/attempt ID would reuse the stale branch (via
// PrepareAttempt's BranchExists path) and seed a new attempt from discarded
// work instead of the intended base.
func TestCleanupAttempts_DeletesBranchRef(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)
	tk := mustCreateAttemptTask(t, h, "branch-ref cleanup task")

	dir, branch, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt: %v", err)
	}
	commitFile(t, dir, "discarded.txt", "discarded work\n", "discarded attempt commit")

	if !project.BranchExists(context.Background(), h.proj.ClonePath, branch) {
		t.Fatalf("attempt branch %q should exist before cleanup", branch)
	}

	h.m.CleanupAttempts(context.Background(), tk, []string{"attempt_1"})

	if project.BranchExists(context.Background(), h.proj.ClonePath, branch) {
		t.Fatalf("attempt branch %q still exists after CleanupAttempts — stale ref would be reused by a later cycle", branch)
	}

	// A later cycle's attempt must start fresh from the base, not inherit the
	// discarded commit off the leaked branch.
	dir2, _, err := h.m.PrepareAttempt(context.Background(), tk, "attempt_1")
	if err != nil {
		t.Fatalf("PrepareAttempt (second cycle): %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir2, "discarded.txt")); statErr == nil {
		t.Errorf("re-prepared attempt inherited discarded.txt from stale branch — cleanup did not reset the ref")
	}
}

func mustCreateAttemptTask(t *testing.T, h preparedHarness, title string) task.Task {
	t.Helper()
	tk, err := h.tasks.Store().Create(title, "", "headless")
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
	return tk
}
