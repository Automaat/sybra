package monitor

import (
	"context"
	"testing"

	"github.com/Automaat/sybra/internal/task"
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
	rem := newRemediator(ft)
	a := Anomaly{Kind: KindLostAgent, TaskID: "task1"}

	label, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if label == "" {
		t.Fatal("expected non-empty label")
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
	rem := newRemediator(ft)
	a := Anomaly{Kind: KindLostAgent, TaskID: "task2"}

	_, err := rem.Apply(context.Background(), a)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(ft.runUpdates) != 0 {
		t.Errorf("want 0 run updates when no running agent run, got %d", len(ft.runUpdates))
	}
}

func TestRemediator_PlanReviewStuck_SetsHumanRequired(t *testing.T) {
	t.Parallel()
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("pr1", task.StatusPlanReview),
	}}
	rem := newRemediator(ft)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "pr1",
		Evidence: map[string]any{
			"status": "plan-review",
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
	if u.id != "pr1" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status == nil || *u.u.Status != task.StatusHumanRequired {
		t.Errorf("status = %v, want human-required", u.u.Status)
	}
	if u.u.StatusReason == nil || *u.u.StatusReason == "" {
		t.Error("status_reason should be set")
	}
}

func TestRemediator_StuckHumanBlocked_HumanRequired_PreservesStatusReason(t *testing.T) {
	t.Parallel()
	existing := mkTask("hr1", task.StatusHumanRequired, func(t *task.Task) {
		t.StatusReason = "waiting for credentials from ops team"
	})
	ft := &fakeTasks{tasks: []task.Task{existing}}
	rem := newRemediator(ft)
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

func TestRemediator_StuckHumanBlocked_UnknownStatus_Errors(t *testing.T) {
	t.Parallel()
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("other1", task.StatusInProgress),
	}}
	rem := newRemediator(ft)
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

func TestIsPlanReviewStuck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a    Anomaly
		want bool
	}{
		{
			name: "plan-review stuck",
			a:    Anomaly{Kind: KindStuckHumanBlocked, Evidence: map[string]any{"status": "plan-review"}},
			want: true,
		},
		{
			name: "human-required stuck",
			a:    Anomaly{Kind: KindStuckHumanBlocked, Evidence: map[string]any{"status": "human-required"}},
			want: false,
		},
		{
			name: "different kind",
			a:    Anomaly{Kind: KindLostAgent, Evidence: map[string]any{"status": "plan-review"}},
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
			if got := isPlanReviewStuck(tc.a); got != tc.want {
				t.Errorf("isPlanReviewStuck = %v, want %v", got, tc.want)
			}
		})
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
