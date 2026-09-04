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
		return github.MyReviewState{Submitted: true, Approved: true, ViewerIsBot: true, ReviewID: 555, ReviewedSHA: "e57e4b5"}, nil
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
		return github.MyReviewState{Submitted: true, Approved: true, ViewerIsBot: true, ReviewID: 555, ReviewedSHA: "e57e4b5"}, nil
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

// A running (possibly stuck/looping) review agent short-circuits the phase
// computation to "reviewing", but a bogus approval it already submitted must
// still be reversed on GitHub rather than left live for the whole run (#2198).
func TestReconcileReviewTask_RunningAgentStillDismissesSelfApproval(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "reviewing")

	claim, ok := r.agents.TryClaimDispatch(tk.ID)
	if !ok {
		t.Fatal("claim dispatch")
	}
	defer claim.Release()
	if !r.agents.HasRunningAgentForTask(tk.ID) {
		t.Fatal("test setup: task should read as running")
	}

	var dismissedReviewID int64
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, Approved: true, ViewerIsBot: true, ReviewID: 777}, nil
	}
	r.dismissReviewFn = func(_ string, _ int, reviewID int64, _ string) error {
		dismissedReviewID = reviewID
		return nil
	}

	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	approved := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE"},
	}

	got := mustGet(t, tasks, tk.ID)
	r.reconcileReviewTask(&got, map[string]github.PullRequest{}, approved)

	if dismissedReviewID != 777 {
		t.Fatalf("dismiss review ID = %d, want 777 — a running agent's stale approval was left live", dismissedReviewID)
	}
	if final := mustGet(t, tasks, tk.ID); final.ReviewPhase != ReviewPhaseReviewing {
		t.Errorf("review_phase = %q, want %q — the running-agent short-circuit still applies", final.ReviewPhase, ReviewPhaseReviewing)
	}
}

// A conflicting PR short-circuits to the conflict phase, but a self-approval is
// still a live green light that native auto-merge would count once the conflict
// clears — it must be reversed now, not on the poll where mergeability flips.
func TestReconcileReviewTask_ConflictStillDismissesSelfApproval(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "conflict")

	var dismissedReviewID int64
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, Approved: true, ViewerIsBot: true, ReviewID: 888}, nil
	}
	r.dismissReviewFn = func(_ string, _ int, reviewID int64, _ string) error {
		dismissedReviewID = reviewID
		return nil
	}

	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	approved := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "CONFLICTING"},
	}

	got := mustGet(t, tasks, tk.ID)
	r.reconcileReviewTask(&got, map[string]github.PullRequest{}, approved)

	if dismissedReviewID != 888 {
		t.Fatalf("dismiss review ID = %d, want 888 — a conflicting PR's self-approval was left live", dismissedReviewID)
	}
	if final := mustGet(t, tasks, tk.ID); final.ReviewPhase != ReviewPhaseConflict {
		t.Errorf("review_phase = %q, want %q — the conflict short-circuit still applies", final.ReviewPhase, ReviewPhaseConflict)
	}
}

// When Sybra is authenticated with a human PAT, a browser approval and an
// agent-submitted approval have the same GitHub login. Outcome-only detection
// cannot distinguish them, so the self-approval backstop must not dismiss
// personal-account approvals as if they were bot approvals.
func TestReconcileReviewTask_HumanViewerApprovalIsNotDismissed(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	dismissCalled := false
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, Approved: true, ReviewID: 999, ReviewedSHA: "e57e4b5"}, nil
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
		t.Fatal("human viewer approval was dismissed as a bot self-approval")
	}
	final := mustGet(t, tasks, tk.ID)
	if final.ReviewPhase != ReviewPhaseApproved {
		t.Errorf("review_phase = %q, want %q", final.ReviewPhase, ReviewPhaseApproved)
	}
	if final.Status != task.StatusInReview {
		t.Errorf("status = %q, want %q", final.Status, task.StatusInReview)
	}
}

// The reviewed-by:@me search leg (inApproved) is the only signal in some polls.
// If REST has not observed a submitted viewer review, treat it as a fallback
// self-approval signal. Without a REST-fetched review ID there is nothing to
// dismiss yet, but the task must still never read as "approved".
func TestReconcileReviewTask_SelfApprovalFromSearchLegWithoutReviewID(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "reviewing")

	dismissCalled := false
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{ViewerIsBot: true, ReviewedSHA: "e57e4b5"}, nil
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

// If REST sees a submitted viewer verdict, it is more precise than the
// reviewed-by:@me search leg. A later CHANGES_REQUESTED/COMMENTED verdict must
// not be overwritten by stale search-leg approval.
func TestReconcileReviewTask_SearchLegApprovalDoesNotOverrideSubmittedRESTVerdict(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	dismissCalled := false
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{Submitted: true, Approved: false, ViewerIsBot: true, ReviewedSHA: "e57e4b5"}, nil
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
		t.Fatal("stale search-leg approval was dismissed as a live bot self-approval")
	}
	final := mustGet(t, tasks, tk.ID)
	if final.ReviewPhase != ReviewPhaseAwaitingAuthor {
		t.Errorf("review_phase = %q, want %q", final.ReviewPhase, ReviewPhaseAwaitingAuthor)
	}
}
