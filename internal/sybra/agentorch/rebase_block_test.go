package agentorch

import (
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
