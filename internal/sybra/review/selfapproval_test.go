package review

import (
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// A self-approval must never read as "approved and waiting to merge" — it is
// always an anomaly (an escaped prompt, a bypassed gh shim, or manual gh use
// under the bot's own credentials). reconcileReviewTask must dismiss it and
// escalate to human-required, not park it as if a human had approved (#2198).
func TestReconcileReviewTask_DismissesSelfApprovalAndEscalates(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "reviewing")

	var dismissedRepo string
	var dismissedNumber int
	var dismissedReviewID int64
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, Approved: true, ReviewID: 555, ReviewedSHA: "e57e4b5"}, nil
	}
	r.dismissReviewFn = func(repo string, number int, reviewID int64, message string) error {
		dismissedRepo, dismissedNumber, dismissedReviewID = repo, number, reviewID
		if message == "" {
			t.Error("dismiss message is empty; the GitHub audit trail would carry no explanation")
		}
		return nil
	}

	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	requested := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE", HeadSHA: "e57e4b5"},
	}

	got := mustGet(t, tasks, tk.ID)
	r.reconcileReviewTask(&got, requested, map[string]github.PullRequest{})

	if dismissedRepo != tk.ProjectID || dismissedNumber != tk.PRNumber || dismissedReviewID != 555 {
		t.Fatalf("dismiss called with (%q, %d, %d), want (%q, %d, 555)",
			dismissedRepo, dismissedNumber, dismissedReviewID, tk.ProjectID, tk.PRNumber)
	}

	final := mustGet(t, tasks, tk.ID)
	if final.ReviewPhase != ReviewPhaseSelfApprovalBlocked {
		t.Errorf("review_phase = %q, want %q", final.ReviewPhase, ReviewPhaseSelfApprovalBlocked)
	}
	if final.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want %q — a self-approval must not be treated as a legitimate approval",
			final.Status, task.StatusHumanRequired)
	}
}

// A dismissal failure (GitHub API error, permissions, etc.) must not stop the
// task from escalating — the important invariant is that the task never
// parks as "approved", whether or not the reversal on GitHub itself succeeds.
func TestReconcileReviewTask_DismissFailureStillEscalates(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "reviewing")

	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, Approved: true, ReviewID: 555, ReviewedSHA: "e57e4b5"}, nil
	}
	r.dismissReviewFn = func(string, int, int64, string) error {
		return errors.New("gh api: HTTP 403")
	}

	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	requested := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE", HeadSHA: "e57e4b5"},
	}

	got := mustGet(t, tasks, tk.ID)
	r.reconcileReviewTask(&got, requested, map[string]github.PullRequest{})

	final := mustGet(t, tasks, tk.ID)
	if final.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want %q — a dismiss failure must not block escalation",
			final.Status, task.StatusHumanRequired)
	}
}

// The reviewed-by:@me search leg (inApproved) is the only signal in some
// polls — e.g. right after the REST fetch already observed a later verdict.
// Without a REST-fetched review ID there is nothing to dismiss yet, but the
// task must still never read as "approved".
func TestReconcileReviewTask_SelfApprovalFromSearchLegWithoutReviewID(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "reviewing")

	dismissCalled := false
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, ReviewedSHA: "e57e4b5"}, nil
	}
	r.dismissReviewFn = func(string, int, int64, string) error {
		dismissCalled = true
		return nil
	}

	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	approved := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE", HeadSHA: "e57e4b5"},
	}

	got := mustGet(t, tasks, tk.ID)
	r.reconcileReviewTask(&got, map[string]github.PullRequest{}, approved)

	if dismissCalled {
		t.Error("dismiss called without a review ID to dismiss")
	}
	final := mustGet(t, tasks, tk.ID)
	if final.ReviewPhase != ReviewPhaseSelfApprovalBlocked {
		t.Errorf("review_phase = %q, want %q even without a dismissable review ID",
			final.ReviewPhase, ReviewPhaseSelfApprovalBlocked)
	}
}
