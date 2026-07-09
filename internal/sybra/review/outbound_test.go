package review

import (
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

func TestExhaustedFixReasonKind(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   github.PRIssueKind
		wantOK bool
	}{
		{"ci_failure", "pr-monitor: auto-fix exhausted after 3 attempts (ci_failure) — needs a human", github.PRIssueCIFailure, true},
		{"conflict", "pr-monitor: auto-fix exhausted after 3 attempts (conflict) — needs a human", github.PRIssueConflict, true},
		{"empty", "", "", false},
		{"unrelated reason", "DCO check failing — needs a human to amend history", "", false},
		{"missing parens", "pr-monitor: auto-fix exhausted after 3 attempts — needs a human", "", false},
		{"empty parens", "pr-monitor: auto-fix exhausted after 3 attempts () — needs a human", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := exhaustedFixReasonKind(tt.reason)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("exhaustedFixReasonKind(%q) = (%q, %v), want (%q, %v)", tt.reason, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestHumanRequiredBlockerReconcileEligible(t *testing.T) {
	ciReason := "pr-monitor: auto-fix exhausted after 3 attempts (ci_failure) — needs a human"
	tests := []struct {
		name string
		task *task.Task
		want bool
	}{
		{
			name: "eligible ci_failure exhaustion",
			task: &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason},
			want: true,
		},
		{
			name: "not human-required",
			task: &task.Task{Status: task.StatusInReview, PRNumber: 42, StatusReason: ciReason},
			want: false,
		},
		{
			name: "no PR linked",
			task: &task.Task{Status: task.StatusHumanRequired, StatusReason: ciReason},
			want: false,
		},
		{
			name: "conflict exhaustion is not reconciled here",
			task: &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: "pr-monitor: auto-fix exhausted after 3 attempts (conflict) — needs a human"},
			want: false,
		},
		{
			name: "draft review reason requires a human",
			task: &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: "Draft review ready — verify & submit on GitHub"},
			want: false,
		},
		{
			name: "review-tagged task is inbound",
			task: &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "chat task never own-PR",
			task: &task.Task{TaskType: task.TaskTypeChat, Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanRequiredBlockerReconcileEligible(tt.task); got != tt.want {
				t.Errorf("humanRequiredBlockerReconcileEligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

// mkExhaustedCITask creates a task parked human-required on an exhausted
// ci_failure fix — the shape reconcileHumanRequiredBlockers targets.
func mkExhaustedCITask(t *testing.T, tasks *task.Manager, prNumber int) task.Task {
	t.Helper()
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("pr-monitor: auto-fix exhausted after 3 attempts (ci_failure) — needs a human"),
		PRNumber:     task.Ptr(prNumber),
		ProjectID:    task.Ptr("Automaat/sybra"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return updated
}

func TestReconcileHumanRequiredBlockersClearsOnCleanPR(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkExhaustedCITask(t, tasks, 42)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Errorf("status = %q, want in-review", got.Status)
	}
	if got.StatusReason != "" {
		t.Errorf("status reason = %q, want cleared", got.StatusReason)
	}
}

func TestReconcileHumanRequiredBlockersStaysParkedWhileCIStillFailing(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkExhaustedCITask(t, tasks, 42)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "FAILURE"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersStaysParkedOnFreshConflict(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkExhaustedCITask(t, tasks, 42)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	// CI cleared, but the PR picked up a merge conflict in the meantime — still
	// blocked, must not be resurrected into an in-review state that doesn't
	// reflect the live blocker.
	prs := []github.PullRequest{{Number: 42, Mergeable: "CONFLICTING", CIStatus: "SUCCESS"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersIgnoresUnrelatedHumanRequiredReasons(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	parked, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("Draft review ready — verify & submit on GitHub"),
		PRNumber:     task.Ptr(42),
		ProjectID:    task.Ptr("Automaat/sybra"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required (reason not machine-checkable)", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersSkipsWhenPRNotFound(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkExhaustedCITask(t, tasks, 42)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	// PR not present in this poll's monitoredPRs snapshot (e.g. closed/merged
	// or not yet surfaced) — leave parked, re-probe next cycle.
	r.reconcileHumanRequiredBlockers(all, nil)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
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
