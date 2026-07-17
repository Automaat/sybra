package review

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

func newReconcileFailureHandler(t *testing.T) (*Handler, *task.Manager) {
	t.Helper()
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	return &Handler{
		logger: logger,
		tasks:  tasks,
		agents: agents,
	}, tasks
}

func newReviewTaskInPhase(t *testing.T, tasks *task.Manager, phase string) task.Task {
	t.Helper()
	tk, err := tasks.Create("Review: external PR", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := tasks.UpdateMap(tk.ID, map[string]any{
		"status":       string(task.StatusInReview),
		"tags":         []string{"review"},
		"project_id":   "Automaat/lightroom-mcp",
		"pr_number":    151,
		"review_phase": phase,
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

// The #2164 shape: a permanently-failing reconcile read left review_phase
// frozen at needs-approval — a *dispatchable* phase — and only ever logged a
// warning. It warned every ~2 minutes for 23 hours while re-reviewing a
// stranger's PR 112 times. Past the limit the task must land somewhere a human
// sees and the dispatcher will not fire on.
func TestRecordReconcileFailure_EscalatesPersistentFailure(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	// The exact error from the incident.
	err := errors.New("resolve viewer login for Automaat/lightroom-mcp#151: gh api user: HTTP 403")
	for range reconcileFailureLimit {
		got, gerr := tasks.Get(tk.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		r.recordReconcileFailure(&got, err)
	}

	got, gerr := tasks.Get(tk.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q after %d consecutive failures — a warn-log is not an alarm",
			got.Status, task.StatusHumanRequired, reconcileFailureLimit)
	}
	if got.StatusReason == "" {
		t.Error("StatusReason is empty; the operator cannot tell why this stopped")
	}
}

// Below the limit the task is left alone: a couple of failed reads is not
// evidence of a defect, and flipping a healthy task to human-required on the
// first blip would be its own bug.
func TestRecordReconcileFailure_ToleratesFailuresBelowLimit(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	err := errors.New("resolve viewer login: HTTP 403")
	for range reconcileFailureLimit - 1 {
		got, gerr := tasks.Get(tk.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		r.recordReconcileFailure(&got, err)
	}

	got, gerr := tasks.Get(tk.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q — escalated before the limit", got.Status, task.StatusInReview)
	}
}

// A 5xx or a timeout is expected weather, not a defect. Counting it would
// escalate healthy tasks during a GitHub blip.
func TestRecordReconcileFailure_TransientNeverEscalates(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	transient := fmt.Errorf("gh: HTTP 502 Bad Gateway")
	for range reconcileFailureLimit * 3 {
		got, gerr := tasks.Get(tk.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		r.recordReconcileFailure(&got, transient)
	}

	got, gerr := tasks.Get(tk.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q — a transient blip must not escalate", got.Status, task.StatusInReview)
	}
	r.failureMu.Lock()
	n := r.reconcileFailures[tk.ID]
	r.failureMu.Unlock()
	if n != 0 {
		t.Errorf("transient failures counted %d toward the limit, want 0", n)
	}
}

// The counter is consecutive: one good read means the defect is gone.
func TestClearReconcileFailure_ResetsTheCount(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	err := errors.New("resolve viewer login: HTTP 403")
	for range reconcileFailureLimit - 1 {
		got, gerr := tasks.Get(tk.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		r.recordReconcileFailure(&got, err)
	}
	r.clearReconcileFailure(tk.ID)

	// A further failure starts from scratch, so this must not escalate.
	got, gerr := tasks.Get(tk.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	r.recordReconcileFailure(&got, err)

	got, gerr = tasks.Get(tk.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q — the counter did not reset on a successful read",
			got.Status, task.StatusInReview)
	}
}

// The wiring, not just the helper: reconcileReviewTask must actually route a
// failed read through the circuit. Without this the call site could be reverted
// to the incident's warn-and-return and every other test here would still pass.
func TestReconcileReviewTask_RoutesFailureThroughCircuit(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	// The exact failure from #2164, repeated until the circuit should trip.
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{}, errors.New("resolve viewer login for Automaat/lightroom-mcp#151: gh api user: HTTP 403")
	}
	// MERGEABLE keeps stickyConflictPhase from deciding early and avoids a live
	// FetchPRState call, so the read under test is the one that fails.
	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	requested := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE", HeadSHA: "e57e4b5"},
	}

	for range reconcileFailureLimit {
		got, gerr := tasks.Get(tk.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		r.reconcileReviewTask(&got, requested, map[string]github.PullRequest{})
	}

	got, gerr := tasks.Get(tk.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q — reconcileReviewTask swallowed a permanently failing read, "+
			"leaving the task dispatchable (the #2164 loop)", got.Status, task.StatusHumanRequired)
	}
}

// A healthy read must leave the task alone and clear any prior failures.
func TestReconcileReviewTask_SuccessClearsTheCircuit(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	var fail bool
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		if fail {
			return github.MyReviewState{}, errors.New("resolve viewer login: HTTP 403")
		}
		return github.MyReviewState{Submitted: true, ReviewedSHA: "e57e4b5"}, nil
	}
	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	requested := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE", HeadSHA: "e57e4b5"},
	}

	// Fail almost to the limit, then succeed — the counter must reset.
	fail = true
	for range reconcileFailureLimit - 1 {
		got, _ := tasks.Get(tk.ID)
		r.reconcileReviewTask(&got, requested, map[string]github.PullRequest{})
	}
	fail = false
	got, _ := tasks.Get(tk.ID)
	r.reconcileReviewTask(&got, requested, map[string]github.PullRequest{})

	r.failureMu.Lock()
	n := r.reconcileFailures[tk.ID]
	r.failureMu.Unlock()
	if n != 0 {
		t.Errorf("failure count = %d after a successful read, want 0", n)
	}

	fail = true
	got, _ = tasks.Get(tk.ID)
	r.reconcileReviewTask(&got, requested, map[string]github.PullRequest{})
	got, _ = tasks.Get(tk.ID)
	if got.Status == task.StatusHumanRequired {
		t.Error("escalated on the first failure after a success; the counter did not reset")
	}
}

// Escalation must be idempotent. human-required is not terminal, so the poller
// keeps feeding the escalated task back in — re-escalating would overwrite the
// operator's own triage note and rewrite updated_at forever on a task nobody is
// working on (also defeating any dwell detector keyed on it).
func TestRecordReconcileFailure_DoesNotClobberOperatorNote(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	err := errors.New("resolve viewer login: HTTP 403")
	for range reconcileFailureLimit {
		got, _ := tasks.Get(tk.ID)
		r.recordReconcileFailure(&got, err)
	}
	escalated, _ := tasks.Get(tk.ID)
	if escalated.Status != task.StatusHumanRequired {
		t.Fatalf("precondition: want escalated, got %q", escalated.Status)
	}

	const note = "OPERATOR: investigating, do not touch"
	if _, uerr := tasks.Update(tk.ID, task.Update{StatusReason: task.Ptr(note)}); uerr != nil {
		t.Fatal(uerr)
	}
	before, _ := tasks.Get(tk.ID)

	// Keep polling with the same failing read.
	for range reconcileFailureLimit * 2 {
		got, _ := tasks.Get(tk.ID)
		r.recordReconcileFailure(&got, err)
	}

	after, _ := tasks.Get(tk.ID)
	if after.StatusReason != note {
		t.Errorf("StatusReason = %q, want the operator's note %q — re-escalation clobbered it",
			after.StatusReason, note)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("updated_at was rewritten (%v -> %v) on an already-escalated task",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// The count must decay when a cycle proves the task is healthy. Otherwise stale
// failures accumulate across hours of a perfectly healthy PR and a single fresh
// blip trips a circuit meant to catch a *persistent* failure.
func TestReconcileReviewTask_HealthyCycleClearsStaleFailures(t *testing.T) {
	r, tasks := newReconcileFailureHandler(t)
	tk := newReviewTaskInPhase(t, tasks, "needs-approval")

	err := errors.New("resolve viewer login: HTTP 403")
	r.fetchMyReviewStateFn = func(string, int) (github.MyReviewState, error) {
		return github.MyReviewState{}, err
	}
	key := reviewPRKey(tk.ProjectID, tk.PRNumber)
	mergeableReq := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "MERGEABLE", HeadSHA: "e57e4b5"},
	}

	// Accumulate failures just short of the limit.
	for range reconcileFailureLimit - 1 {
		got, _ := tasks.Get(tk.ID)
		r.reconcileReviewTask(&got, mergeableReq, map[string]github.PullRequest{})
	}

	// The PR goes CONFLICTING: a decided phase proves GitHub is readable, so the
	// stale failures must not survive it.
	conflicting := map[string]github.PullRequest{
		key: {Number: tk.PRNumber, Repository: tk.ProjectID, Mergeable: "CONFLICTING", HeadSHA: "e57e4b5"},
	}
	got, _ := tasks.Get(tk.ID)
	r.reconcileReviewTask(&got, conflicting, map[string]github.PullRequest{})

	r.failureMu.Lock()
	n := r.reconcileFailures[tk.ID]
	r.failureMu.Unlock()
	if n != 0 {
		t.Fatalf("failure count = %d after a healthy cycle, want 0 — one later blip would escalate a healthy task", n)
	}
}
