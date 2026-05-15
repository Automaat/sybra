package monitor

import (
	"context"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

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

func TestRemediator_StuckHumanBlocked_NonPlanReview_Errors(t *testing.T) {
	t.Parallel()
	ft := &fakeTasks{tasks: []task.Task{
		mkTask("hr1", task.StatusHumanRequired),
	}}
	rem := newRemediator(ft)
	a := Anomaly{
		Kind:   KindStuckHumanBlocked,
		TaskID: "hr1",
		Evidence: map[string]any{
			"status": "human-required",
		},
	}
	_, err := rem.Apply(context.Background(), a)
	if err == nil {
		t.Fatal("expected error for non-plan-review stuck_human_blocked")
	}
	if len(ft.updates) != 0 {
		t.Fatalf("want 0 updates for non-plan-review, got %d", len(ft.updates))
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
