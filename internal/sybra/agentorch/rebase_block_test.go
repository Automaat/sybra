package agentorch

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

func newRebaseBlockTestManager(t *testing.T) *task.Manager {
	t.Helper()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return task.NewManager(store, nil)
}

func discardSlogLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestMarkRebaseBlocked_ReProbesResolvedRemotePR pins the second half of the
// bug report: when there is no autonomous conflict-recovery callback (or it
// declined), a rebase failure must still re-probe the task's linked PR before
// parking human-required — an external bot may have already force-pushed a
// green fix onto a PR the local worktree merely diverged from.
func TestMarkRebaseBlocked_ReProbesResolvedRemotePR(t *testing.T) {
	orig := fetchPRStateForRebaseBlock
	defer func() { fetchPRStateForRebaseBlock = orig }()
	fetchPRStateForRebaseBlock = func(repo string, number int) (github.PRState, error) {
		if repo != "acme/widgets" || number != 42 {
			t.Fatalf("unexpected fetch: %s#%d", repo, number)
		}
		return github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}, nil
	}

	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("pr-fix task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "acme/widgets"
	prNumber := 42
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: &projectID, PRNumber: &prNumber}); err != nil {
		t.Fatal(err)
	}

	handled := MarkRebaseBlocked(tasks, tk.ID, worktree.ErrRebaseFailed, discardSlogLogger(), nil)
	if !handled {
		t.Fatal("MarkRebaseBlocked = false, want true (handled)")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInReview)
	}
}

// TestMarkRebaseBlocked_ParksHumanRequiredWhenNotResolved is the control:
// an unresolved remote PR (or fetch failure) still parks human-required, and
// a nil recoverConflict is not treated as automatic recovery.
func TestMarkRebaseBlocked_ParksHumanRequiredWhenNotResolved(t *testing.T) {
	orig := fetchPRStateForRebaseBlock
	defer func() { fetchPRStateForRebaseBlock = orig }()
	fetchPRStateForRebaseBlock = func(repo string, number int) (github.PRState, error) {
		return github.PRState{State: "OPEN", Mergeable: "CONFLICTING"}, nil
	}

	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("pr-fix task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "acme/widgets"
	prNumber := 42
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: &projectID, PRNumber: &prNumber}); err != nil {
		t.Fatal(err)
	}

	handled := MarkRebaseBlocked(tasks, tk.ID, worktree.ErrRebaseFailed, discardSlogLogger(), nil)
	if !handled {
		t.Fatal("MarkRebaseBlocked = false, want true (handled)")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != worktreeerr.RebaseBlockedReason {
		t.Fatalf("reason = %q, want %q", got.StatusReason, worktreeerr.RebaseBlockedReason)
	}
}

// TestMarkRebaseBlocked_RespectsRecoverConflictsOwnExhaustionReason proves the
// fix for task bdcc90a4: when recoverConflict declines but has already parked
// the task human-required with its own specific reason (e.g.
// review.Handler.markConflictRecoveryExhausted's attempt-count message), the
// generic worktreeerr.RebaseBlockedReason below must not overwrite it — an
// operator (or the automated human-review agent) needs the attempt-count
// detail to tell an exhausted recovery loop apart from a fresh conflict.
func TestMarkRebaseBlocked_RespectsRecoverConflictsOwnExhaustionReason(t *testing.T) {
	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("exhausted recovery task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	const specificReason = "branch conflict recovery attempted 3 time(s) and failed: resolve conflicts or recreate the task branch"
	recoverConflict := func(taskID string) bool {
		if _, err := tasks.Update(taskID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: task.Ptr(specificReason),
		}); err != nil {
			t.Fatal(err)
		}
		return false
	}

	handled := MarkRebaseBlocked(tasks, tk.ID, worktree.ErrRebaseFailed, discardSlogLogger(), recoverConflict)
	if !handled {
		t.Fatal("MarkRebaseBlocked = false, want true (handled)")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != specificReason {
		t.Fatalf("reason = %q, want the specific exhaustion reason %q (must not be overwritten by the generic reason)", got.StatusReason, specificReason)
	}
}

func TestMarkRebaseBlocked_ReProbesResolvedPRAfterRecoveryExhaustion(t *testing.T) {
	orig := fetchPRStateForRebaseBlock
	defer func() { fetchPRStateForRebaseBlock = orig }()
	fetchPRStateForRebaseBlock = func(repo string, number int) (github.PRState, error) {
		if repo != "acme/widgets" || number != 42 {
			t.Fatalf("unexpected fetch: %s#%d", repo, number)
		}
		return github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}, nil
	}

	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("resolved after exhausted recovery task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "acme/widgets"
	prNumber := 42
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: &projectID, PRNumber: &prNumber}); err != nil {
		t.Fatal(err)
	}

	const specificReason = "branch conflict recovery attempted 3 time(s) and failed: resolve conflicts or recreate the task branch"
	recoverConflict := func(taskID string) bool {
		if _, err := tasks.Update(taskID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: task.Ptr(specificReason),
		}); err != nil {
			t.Fatal(err)
		}
		return false
	}

	handled := MarkRebaseBlocked(tasks, tk.ID, worktree.ErrRebaseFailed, discardSlogLogger(), recoverConflict)
	if !handled {
		t.Fatal("MarkRebaseBlocked = false, want true (handled)")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q after resolved PR re-probe", got.Status, task.StatusInReview)
	}
	wantReason := "worktree rebase-blocked: PR #42 already resolved on remote"
	if got.StatusReason != wantReason {
		t.Fatalf("reason = %q, want %q", got.StatusReason, wantReason)
	}
}

// TestMarkRebaseBlocked_NoLinkedPRParksHumanRequired covers a task with no
// project/PR: the resolved-probe must be skipped, not mistaken for resolved.
func TestMarkRebaseBlocked_NoLinkedPRParksHumanRequired(t *testing.T) {
	orig := fetchPRStateForRebaseBlock
	defer func() { fetchPRStateForRebaseBlock = orig }()
	fetchPRStateForRebaseBlock = func(repo string, number int) (github.PRState, error) {
		t.Fatal("fetchPRStateForRebaseBlock should not be called without a linked PR")
		return github.PRState{}, nil
	}

	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("no pr task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	handled := MarkRebaseBlocked(tasks, tk.ID, worktree.ErrRebaseFailed, discardSlogLogger(), nil)
	if !handled {
		t.Fatal("MarkRebaseBlocked = false, want true (handled)")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
}

// TestMarkRebaseBlocked_DiskSpaceErrorSkipsRecoveryAndPRProbe guards sybra#1856:
// an ENOSPC failure wrapped in ErrRebaseFailed must surface the real root
// cause (host disk space exhausted) instead of the generic branch-stale
// reason, and must never be routed into conflict recovery or the PR
// already-resolved re-probe — neither is relevant to a full disk.
func TestMarkRebaseBlocked_DiskSpaceErrorSkipsRecoveryAndPRProbe(t *testing.T) {
	orig := fetchPRStateForRebaseBlock
	defer func() { fetchPRStateForRebaseBlock = orig }()
	fetchPRStateForRebaseBlock = func(repo string, number int) (github.PRState, error) {
		t.Fatal("fetchPRStateForRebaseBlock should not be called for a disk-space error")
		return github.PRState{}, nil
	}

	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("disk space task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "acme/widgets"
	prNumber := 42
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: &projectID, PRNumber: &prNumber}); err != nil {
		t.Fatal(err)
	}

	recoveryCalled := false
	diskSpaceErr := fmt.Errorf("%w: reconcile branch with remote: %w", worktree.ErrRebaseFailed, errors.New("cannot open 'FETCH_HEAD': No space left on device"))
	handled := MarkRebaseBlocked(tasks, tk.ID, diskSpaceErr, discardSlogLogger(), func(string) bool {
		recoveryCalled = true
		return true
	})
	if !handled {
		t.Fatal("MarkRebaseBlocked = false, want true (handled)")
	}
	if recoveryCalled {
		t.Fatal("conflict recovery was invoked for a disk-space failure")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != worktreeerr.DiskSpaceExhaustedReason {
		t.Fatalf("reason = %q, want %q", got.StatusReason, worktreeerr.DiskSpaceExhaustedReason)
	}
}

func TestMarkRebaseBlocked_IgnoresTransientFetch(t *testing.T) {
	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("transient fetch task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	called := false
	handled := MarkRebaseBlocked(tasks, tk.ID, worktree.ErrTransientFetch, discardSlogLogger(), func(string) bool {
		called = true
		return true
	})
	if handled {
		t.Fatal("MarkRebaseBlocked = true, want false for transient fetch")
	}
	if called {
		t.Fatal("recoverConflict should not be called for transient fetch")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("status = %q, want unchanged %q", got.Status, task.StatusTodo)
	}
}

// TestMarkRebaseBlockedWithRecoveryResult_HandledWithoutRecovery pins the
// handled/recovered distinction sybra#1487's fix depends on: when the rebase
// failure resolves via the already-resolved-remote-PR downgrade (not an
// autonomous conflict-fix redispatch), the call is still fully "handled" —
// callers must not treat handled=false-equivalent (checking only `recovered`)
// and fall through to their own status write, which would clobber the
// in_review status this call already set back to human-required using a
// stale pre-dispatch snapshot.
func TestMarkRebaseBlockedWithRecoveryResult_HandledWithoutRecovery(t *testing.T) {
	orig := fetchPRStateForRebaseBlock
	defer func() { fetchPRStateForRebaseBlock = orig }()
	fetchPRStateForRebaseBlock = func(repo string, number int) (github.PRState, error) {
		return github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}, nil
	}

	tasks := newRebaseBlockTestManager(t)
	tk, err := tasks.Create("pr-fix task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "acme/widgets"
	prNumber := 42
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: &projectID, PRNumber: &prNumber}); err != nil {
		t.Fatal(err)
	}

	handled, recovered := MarkRebaseBlockedWithRecoveryResult(tasks, tk.ID, worktree.ErrRebaseFailed, discardSlogLogger(),
		func(string) bool { return false })
	if !handled {
		t.Fatal("handled = false, want true — the already-resolved downgrade fully applies the status change")
	}
	if recovered {
		t.Fatal("recovered = true, want false — no conflict-fix agent was dispatched")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInReview)
	}
}
