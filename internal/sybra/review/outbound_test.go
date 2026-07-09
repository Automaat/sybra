package review

import (
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// newOutboundTestHandler builds a Handler backed by a temp task store and
// a real (idle) agent manager — enough to exercise the PR-phase reconciler.
func newOutboundTestHandler(t *testing.T) (*Handler, *task.Manager) {
	t.Helper()
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	r := &Handler{
		logger: logger,
		tasks:  tasks,
		agents: agents,
	}
	return r, tasks
}

// mkOwnPRTask creates a task and drives it to in-review with a linked PR.
func mkOwnPRTask(t *testing.T, tasks *task.Manager, prNumber int, tags []string) task.Task {
	t.Helper()
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		PRNumber:  task.Ptr(prNumber),
		ProjectID: task.Ptr("Automaat/sybra"),
		Tags:      &tags,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return updated
}

func TestReconcilePRPhases(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	own := mkOwnPRTask(t, tasks, 42, nil)
	review := mkOwnPRTask(t, tasks, 7, []string{"review"})

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{
		{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"},
		{Number: 7, ReviewDecision: "CHANGES_REQUESTED"},
	}
	r.reconcilePRPhases(all, prs)

	got, err := tasks.Get(own.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRPhase != PRPhaseApproved {
		t.Errorf("own PR phase = %q, want %q", got.PRPhase, PRPhaseApproved)
	}

	// A review-tagged task is inbound — the outbound reconciler must skip it so
	// it stays driven by ReviewPhase, not PRPhase.
	gotReview, err := tasks.Get(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotReview.PRPhase != "" {
		t.Errorf("review task PRPhase = %q, want empty", gotReview.PRPhase)
	}
}

func TestReconcilePRPhasesClearsStaleWhenIneligible(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	// Seed a phase, then move the task out of the In Review column.
	if _, err := tasks.Update(created.ID, task.Update{
		PRPhase: task.Ptr(PRPhaseAwaitingApproval),
		Status:  task.Ptr(task.StatusInProgress),
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRPhase != "" {
		t.Errorf("stale PRPhase = %q, want cleared", got.PRPhase)
	}
}

func TestLinkedOwnPRHumanRequiredDrift(t *testing.T) {
	completedAt := time.Now().UTC()
	ts := completedAt.Add(-time.Minute)
	drifted := &task.Task{
		Status:       task.StatusHumanRequired,
		PRNumber:     42,
		UpdatedAt:    ts,
		StatusReason: "",
		Workflow: &workflow.Execution{
			WorkflowID:  "simple-task-pr",
			State:       workflow.ExecCompleted,
			CompletedAt: &completedAt,
		},
	}
	if !linkedOwnPRHumanRequiredDrift(drifted, true) {
		t.Fatal("expected linked PR drift")
	}
}

func TestReconcilePRPhasesDoesNotReactivateFreshManualHumanRequired(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	completedAt := time.Now().UTC().Add(-500 * time.Millisecond)
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &completedAt,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(""),
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func TestReconcilePRPhasesDoesNotReactivateReasonedHumanRequired(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	reason := "testing infrastructure failed after retry"
	now := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &now,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: &reason,
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
	if got.PRPhase != "" {
		t.Errorf("phase = %q, want unchanged empty", got.PRPhase)
	}
}

func TestReconcilePRPhasesDoesNotReactivateLaterManualHumanRequired(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	completedAt := time.Now().UTC().Add(-5 * time.Minute)
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &completedAt,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(""),
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func TestReconcilePRPhasesDoesNotReactivateWithoutLivePR(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	now := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &now,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(""),
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, nil)

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

// mkHumanRequiredBlockerTask creates a task parked human-required with the
// given PR number, status reason, and tags — the shape
// reconcileHumanRequiredBlockers evaluates.
func mkHumanRequiredBlockerTask(t *testing.T, tasks *task.Manager, prNumber int, reason string, tags []string) task.Task {
	t.Helper()
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		PRNumber:     task.Ptr(prNumber),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Tags:         &tags,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return updated
}

func ciExhaustedReason(kind string) string {
	return "pr-monitor: auto-fix exhausted after 3 attempts (" + kind + ") — needs a human"
}

// prStateFromJSON builds a github.PRState from a raw JSON PR-state document.
// PRState.StatusCheckRollup is a slice of an unexported per-package check
// type, so tests outside internal/github can't construct check entries as Go
// literals — unmarshaling through the same json tags PRState is normally
// decoded from is the only way to populate it from this package.
func prStateFromJSON(raw string) github.PRState {
	var s github.PRState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		panic(err)
	}
	return s
}

func openMergeableGreenPR() github.PRState {
	return prStateFromJSON(`{
		"state": "OPEN",
		"mergeable": "MERGEABLE",
		"statusCheckRollup": [{"__typename": "StatusContext", "name": "ci", "state": "SUCCESS"}]
	}`)
}

func openMergeablePendingPR() github.PRState {
	return prStateFromJSON(`{
		"state": "OPEN",
		"mergeable": "MERGEABLE",
		"statusCheckRollup": [{"__typename": "StatusContext", "name": "ci", "state": "PENDING"}]
	}`)
}

func openMergeableFailedPR() github.PRState {
	return prStateFromJSON(`{
		"state": "OPEN",
		"mergeable": "MERGEABLE",
		"statusCheckRollup": [{"__typename": "StatusContext", "name": "ci", "state": "FAILURE"}]
	}`)
}

func openMergeableNoChecksPR() github.PRState {
	return prStateFromJSON(`{"state": "OPEN", "mergeable": "MERGEABLE", "statusCheckRollup": []}`)
}

func closedPR() github.PRState {
	return prStateFromJSON(`{"state": "CLOSED", "mergeable": "MERGEABLE"}`)
}

func TestReconcileHumanRequiredBlockers(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		tags       []string
		fetchState func(repo string, number int) (github.PRState, error)
		wantStatus task.Status
		wantLatch  bool
	}{
		{
			name:   "ci_failure cleared -> flips to in-review",
			reason: ciExhaustedReason("ci_failure"),
			fetchState: func(string, int) (github.PRState, error) {
				return openMergeableGreenPR(), nil
			},
			wantStatus: task.StatusInReview,
			wantLatch:  true,
		},
		{
			name:   "conflict cleared -> flips to in-review",
			reason: ciExhaustedReason("conflict"),
			fetchState: func(string, int) (github.PRState, error) {
				return openMergeableGreenPR(), nil
			},
			wantStatus: task.StatusInReview,
			wantLatch:  true,
		},
		{
			name:   "CI pending -> stays parked",
			reason: ciExhaustedReason("ci_failure"),
			fetchState: func(string, int) (github.PRState, error) {
				return openMergeablePendingPR(), nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "CI unknown/empty -> stays parked (fails closed)",
			reason: ciExhaustedReason("ci_failure"),
			fetchState: func(string, int) (github.PRState, error) {
				return openMergeableNoChecksPR(), nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "CI failed -> stays parked",
			reason: ciExhaustedReason("ci_failure"),
			fetchState: func(string, int) (github.PRState, error) {
				return openMergeableFailedPR(), nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "PR closed -> stays parked",
			reason: ciExhaustedReason("ci_failure"),
			fetchState: func(string, int) (github.PRState, error) {
				return closedPR(), nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "fetch error -> stays parked",
			reason: ciExhaustedReason("ci_failure"),
			fetchState: func(string, int) (github.PRState, error) {
				return github.PRState{}, errors.New("boom")
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "human-authored reason -> skipped, never probed",
			reason: "please double check the migration by hand",
			fetchState: func(string, int) (github.PRState, error) {
				t.Fatal("must not probe a human-authored reason")
				return github.PRState{}, nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "watchdog reason -> skipped, never probed",
			reason: "watchdog: rate limit",
			fetchState: func(string, int) (github.PRState, error) {
				t.Fatal("must not probe a watchdog reason")
				return github.PRState{}, nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "tamper-flagged reason -> skipped, never probed",
			reason: workflow.TamperFlaggedReasonPrefix + " tests/foo_test.go",
			fetchState: func(string, int) (github.PRState, error) {
				t.Fatal("must not probe a tamper-flagged reason")
				return github.PRState{}, nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "comment-review exhaustion -> skipped, never probed",
			reason: ciExhaustedReason("comments"),
			fetchState: func(string, int) (github.PRState, error) {
				t.Fatal("must not probe a comments exhaustion")
				return github.PRState{}, nil
			},
			wantStatus: task.StatusHumanRequired,
		},
		{
			name:   "already latched -> skipped, never probed",
			reason: ciExhaustedReason("ci_failure"),
			tags:   []string{reconciledLatchTag},
			fetchState: func(string, int) (github.PRState, error) {
				t.Fatal("must not re-probe an already-latched task")
				return github.PRState{}, nil
			},
			wantStatus: task.StatusHumanRequired,
			wantLatch:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, tasks := newOutboundTestHandler(t)
			r.fetchPRStateFn = tt.fetchState
			created := mkHumanRequiredBlockerTask(t, tasks, 42, tt.reason, tt.tags)

			all, err := tasks.List()
			if err != nil {
				t.Fatal(err)
			}
			r.reconcileHumanRequiredBlockers(all)

			got, err := tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tt.wantStatus)
			}
			hasLatch := false
			for _, tag := range got.Tags {
				if tag == reconciledLatchTag {
					hasLatch = true
				}
			}
			if hasLatch != tt.wantLatch {
				t.Errorf("latch tag present = %v, want %v", hasLatch, tt.wantLatch)
			}
			if tt.wantStatus == task.StatusInReview && got.StatusReason != "" {
				t.Errorf("statusReason = %q, want cleared", got.StatusReason)
			}
		})
	}
}

// TestReconcileHumanRequiredBlockersNoDoubleMoveWithReactivateLinkedOwnPR
// exercises reconcilePRPhases (which drives reactivateLinkedOwnPR) and
// reconcileHumanRequiredBlockers back to back, in the same order the two
// poll paths run them, over a task that carries the pr-monitor auto-fix
// exhausted reason. The two repair paths are mutually exclusive by
// construction — reactivateLinkedOwnPR only fires on an *empty*
// statusReason (a workflow-completion race), while the blocker reconciler
// only fires on the exhausted-fix reason text — so running both must leave
// exactly one status write, from the blocker reconciler, with no leftover
// drift-repair side effect.
func TestReconcileHumanRequiredBlockersNoDoubleMoveWithReactivateLinkedOwnPR(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.fetchPRStateFn = func(string, int) (github.PRState, error) {
		return openMergeableGreenPR(), nil
	}

	parked := mkHumanRequiredBlockerTask(t, tasks, 42, ciExhaustedReason("ci_failure"), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	// reactivateLinkedOwnPR must be a no-op here: it only reactivates an
	// empty statusReason, and this task's reason is the exhausted-fix text.
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})
	afterPhases, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPhases.Status != task.StatusHumanRequired {
		t.Fatalf("reconcilePRPhases moved the task prematurely: status = %q", afterPhases.Status)
	}

	all, err = tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcileHumanRequiredBlockers(all)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Errorf("status = %q, want in-review (via reconcileHumanRequiredBlockers)", got.Status)
	}
	if got.StatusReason != "" {
		t.Errorf("statusReason = %q, want cleared", got.StatusReason)
	}
	hasLatch := false
	for _, tag := range got.Tags {
		if tag == reconciledLatchTag {
			hasLatch = true
		}
	}
	if !hasLatch {
		t.Error("expected reconciledLatchTag after blocker reconciliation")
	}
}

func TestApplyPRPhaseSkipsNoOp(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	created := mkOwnPRTask(t, tasks, 42, nil)
	if _, err := tasks.Update(created.ID, task.Update{PRPhase: task.Ptr(PRPhaseDraft)}); err != nil {
		t.Fatal(err)
	}

	cur, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := cur.UpdatedAt

	// Same phase → no write, status untouched.
	r.applyPRPhase(&cur, PRPhaseDraft)
	after, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before) {
		t.Errorf("no-op applyPRPhase wrote the task (updatedAt changed)")
	}
	if after.Status != task.StatusInReview {
		t.Errorf("applyPRPhase changed status to %q", after.Status)
	}
}
