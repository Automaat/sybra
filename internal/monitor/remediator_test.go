package monitor

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestRemediator_LostAgent_MarksRunningRunStopped(t *testing.T) {
	t.Parallel()
	withRun := func(t *task.Task) {
		t.AgentRuns = []task.AgentRun{
			{AgentID: "old-agent", State: "stopped"},
			{AgentID: "lost-agent", State: "running"},
		}
	}
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("task1", task.StatusInProgress, withRun),
	}}
	var recoverCalls int
	rem := newRemediator(ft, func(context.Context, string) { recoverCalls++ }, nil, nil)
	a := Anomaly{Kind: KindLostAgent, TaskID: "task1"}

	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label == "" {
		t.Fatal("expected non-empty label")
	}
	if recoverCalls != 1 {
		t.Fatalf("want 1 recovery call, got %d", recoverCalls)
	}

	// Task must remain in-progress so workflow/recovery can resume it; moving
	// it to todo leaves existing workflows inert.
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 task update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.u.Status != nil {
		t.Errorf("status must be preserved, got %v", *u.u.Status)
	}
	if u.u.StatusReason == nil || *u.u.StatusReason == "" {
		t.Error("status_reason should explain recovery handoff")
	}

	// Running agent run must be marked stopped before the task update.
	if len(ft.runUpdates) != 1 {
		t.Fatalf("want 1 run update, got %d", len(ft.runUpdates))
	}
	ru := ft.runUpdates[0]
	if ru.taskID != "task1" {
		t.Errorf("runUpdate taskID = %q, want task1", ru.taskID)
	}
	if ru.agentID != "lost-agent" {
		t.Errorf("runUpdate agentID = %q, want lost-agent", ru.agentID)
	}
	if ru.patch.State == nil || *ru.patch.State != "stopped" {
		t.Errorf("runUpdate state = %v, want stopped", ru.patch.State)
	}
}

func TestRemediator_LostAgent_NoRunningRun_SkipsRunUpdate(t *testing.T) {
	t.Parallel()
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("task2", task.StatusInProgress),
	}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{Kind: KindLostAgent, TaskID: "task2"}

	_, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(ft.runUpdates) != 0 {
		t.Errorf("want 0 run updates when no running agent run, got %d", len(ft.runUpdates))
	}
}

// A plan-review task must wait indefinitely for the human — the remediator has
// no plan-review action, so a plan-review-shaped anomaly is an error, never an
// escalation to human-required.
func TestRemediator_PlanReviewStuck_NotRemediated(t *testing.T) {
	t.Parallel()
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("pr1", task.StatusPlanReview),
	}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "pr1",
		Evidence: map[string]any{
			"status": "plan-review",
		},
	}
	if _, err := rem.Apply(context.Background(), a); err == nil {
		t.Fatal("expected error: plan-review must not be remediated to human-required")
	}
	if len(ft.updates) != 0 {
		t.Fatalf("want 0 updates (plan-review left untouched), got %d", len(ft.updates))
	}
}

func TestRemediator_StuckHumanBlocked_HumanRequired_PreservesStatusReason(t *testing.T) {
	t.Parallel()
	existing := mkTask("hr1", task.StatusHumanRequired, func(t *task.Task) {
		t.StatusReason = "waiting for credentials from ops team"
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr1",
		Evidence: map[string]any{
			"status": "human-required",
		},
	}
	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label == "" {
		t.Fatal("expected non-empty label")
	}
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.id != "hr1" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status != nil {
		t.Errorf("status must not change, got %v", u.u.Status)
	}
	if u.u.StatusReason != nil {
		t.Errorf("status_reason must not change, got %q", *u.u.StatusReason)
	}
}

func TestRemediator_StuckHumanBlocked_MergedPRCompletesTaskAndWorkflow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	existing := mkTask("hr-merged", task.StatusHumanRequired, func(tk *task.Task) {
		tk.StatusReason = "waiting for human approval"
		tk.ProjectID = "o/r"
		tk.PRNumber = 42
		tk.Workflow = &workflow.Execution{
			WorkflowID:  "simple-task-review",
			CurrentStep: "wait_human",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		}
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(
		ft,
		nil,
		func(repo string, number int) (github.PRState, error) {
			if repo != "o/r" || number != 42 {
				t.Fatalf("fetchPRState(%q, %d)", repo, number)
			}
			return github.PRState{State: "MERGED"}, nil
		},
		func() time.Time { return now },
	)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr-merged",
		Evidence: map[string]any{
			"status": "human-required",
		},
	}

	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label != "stuck_human_blocked:merged:hr-merged" {
		t.Fatalf("label = %q, want merged label", label)
	}
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.u.Status == nil || *u.u.Status != task.StatusDone {
		t.Fatalf("status = %v, want done", u.u.Status)
	}
	if u.u.Outcome == nil || *u.u.Outcome != "merged" {
		t.Fatalf("outcome = %v, want merged", u.u.Outcome)
	}
	if u.u.StatusReason == nil || *u.u.StatusReason != "" {
		t.Fatalf("status_reason = %v, want cleared", u.u.StatusReason)
	}
	if u.u.Workflow == nil || *u.u.Workflow == nil {
		t.Fatal("workflow must be completed in the same update")
	}
	if got := (*u.u.Workflow).State; got != workflow.ExecCompleted {
		t.Fatalf("workflow state = %q, want completed", got)
	}
	if got := (*u.u.Workflow).CurrentStep; got != "" {
		t.Fatalf("workflow CurrentStep = %q, want empty", got)
	}
	if got := (*u.u.Workflow).Variables["cancel_reason"]; got != "monitor: linked PR merged" {
		t.Fatalf("cancel_reason = %q, want merged marker", got)
	}
}

func TestRemediator_StuckHumanBlocked_PRStateErrorFallsBackToRefresh(t *testing.T) {
	t.Parallel()
	existing := mkTask("hr-pr-error", task.StatusHumanRequired, func(tk *task.Task) {
		tk.StatusReason = "waiting for human approval"
		tk.ProjectID = "o/r"
		tk.PRNumber = 42
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(
		ft,
		nil,
		func(repo string, number int) (github.PRState, error) {
			if repo != "o/r" || number != 42 {
				t.Fatalf("fetchPRState(%q, %d)", repo, number)
			}
			return github.PRState{}, errors.New("gh auth unavailable")
		},
		nil,
	)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr-pr-error",
		Evidence: map[string]any{
			"status": "human-required",
		},
	}

	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label != "stuck_human_blocked:hr-pr-error" {
		t.Fatalf("label = %q, want refresh label", label)
	}
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 refresh update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.id != "hr-pr-error" {
		t.Fatalf("updated wrong task: %q", u.id)
	}
	if u.u.Status != nil {
		t.Fatalf("status must not change, got %v", u.u.Status)
	}
	if u.u.StatusReason != nil {
		t.Fatalf("status_reason must not change, got %q", *u.u.StatusReason)
	}
}

func TestRemediator_StuckHumanBlocked_KnownLostAgentCause_AutoRetries(t *testing.T) {
	t.Parallel()
	existing := mkTask("hr2", task.StatusHumanRequired, func(t *task.Task) {
		t.StatusReason = "watchdog: stop"
		t.Tags = []string{"medium"}
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr2",
		Evidence: map[string]any{
			"status":                         "human-required",
			"known_lost_agent_investigation": true,
		},
	}
	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label == "" {
		t.Fatal("expected non-empty label")
	}
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.id != "hr2" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status == nil || *u.u.Status != task.StatusInProgress {
		t.Fatalf("status = %v, want in-progress", u.u.Status)
	}
	if u.u.StatusReason == nil || *u.u.StatusReason == "" {
		t.Error("status_reason should explain the auto-retry")
	}
	if u.u.Tags == nil {
		t.Fatal("expected tags to be updated with the auto-retried marker")
	}
	found := false
	for _, tag := range *u.u.Tags {
		if tag == monitorAutoRetriedTag {
			found = true
		}
	}
	if !found {
		t.Errorf("tags = %v, want to contain %q", *u.u.Tags, monitorAutoRetriedTag)
	}
	// Original tags must be preserved, not clobbered.
	if !slices.Contains(*u.u.Tags, "medium") {
		t.Errorf("tags = %v, want to still contain %q", *u.u.Tags, "medium")
	}
}

func TestRemediator_StuckHumanBlocked_KnownLostAgentCause_HumanVerdictDoesNotRetry(t *testing.T) {
	t.Parallel()
	existing := mkTask("hr3", task.StatusHumanRequired, func(t *task.Task) {
		t.StatusReason = "waiting for human-provided context"
		t.Tags = []string{"medium"}
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr3",
		Evidence: map[string]any{
			"status":                         "human-required",
			"known_lost_agent_investigation": true,
			"human_review_verdict":           "human",
		},
	}

	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label == "" {
		t.Fatal("expected non-empty label")
	}
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.u.Status != nil {
		t.Fatalf("status must not change for human-confirmed block, got %v", u.u.Status)
	}
	if u.u.Tags != nil {
		t.Fatalf("tags must not change for human-confirmed block, got %v", *u.u.Tags)
	}
}

func TestRemediator_StuckHumanBlocked_KnownLostAgentCause_TamperFlagDoesNotRetry(t *testing.T) {
	t.Parallel()
	existing := mkTask("hr4", task.StatusHumanRequired, func(t *task.Task) {
		t.StatusReason = workflow.TamperFlaggedReasonPrefix + " removed coverage in internal/foo_test.go"
		t.Tags = []string{"medium"}
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr4",
		Evidence: map[string]any{
			"status":                         "human-required",
			"known_lost_agent_investigation": true,
		},
	}

	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label == "" {
		t.Fatal("expected non-empty label")
	}
	if len(ft.updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(ft.updates))
	}
	u := ft.updates[0]
	if u.u.Status != nil {
		t.Fatalf("status must not change for tamper-flagged block, got %v", u.u.Status)
	}
	if u.u.Tags != nil {
		t.Fatalf("tags must not change for tamper-flagged block, got %v", *u.u.Tags)
	}
}
func TestRemediator_StuckHumanBlocked_UnknownStatus_Errors(t *testing.T) {
	t.Parallel()
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("other1", task.StatusInProgress),
	}}
	rem := newRemediator(ft, nil, nil, nil)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "other1",
		Evidence: map[string]any{
			"status": "in-progress",
		},
	}
	_, err := rem.Apply(context.Background(), a)
	if err == nil {
		t.Fatal("expected error for unknown-status stuck_human_blocked")
	}
	if len(ft.updates) != 0 {
		t.Fatalf("want 0 updates, got %d", len(ft.updates))
	}
}

func TestIsHumanRequiredStuck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a    Anomaly
		want bool
	}{
		{
			name: "human-required stuck",
			a:    Anomaly{Kind: KindStuckHumanBlocked, Evidence: map[string]any{"status": "human-required"}},
			want: true,
		},
		{
			name: "plan-review stuck",
			a:    Anomaly{Kind: KindStuckHumanBlocked, Evidence: map[string]any{"status": "plan-review"}},
			want: false,
		},
		{
			name: "different kind",
			a:    Anomaly{Kind: KindLostAgent, Evidence: map[string]any{"status": "human-required"}},
			want: false,
		},
		{
			name: "no evidence",
			a:    Anomaly{Kind: KindStuckHumanBlocked},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHumanRequiredStuck(tc.a); got != tc.want {
				t.Errorf("isHumanRequiredStuck = %v, want %v", got, tc.want)
			}
		})
	}
}
