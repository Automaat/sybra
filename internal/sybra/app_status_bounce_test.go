package sybra

import "testing"

import (
	"github.com/Automaat/sybra/internal/task"
)

func TestStatusBounceTrippedRequiresRepeatedReciprocalTransitions(t *testing.T) {
	var a App
	const id = "task-bounce"

	for i := 0; i < statusBounceLimit-1; i++ {
		if a.statusBounceTripped(id, "human-required", "in-review") {
			t.Fatal("tripped before both directions repeated")
		}
		if a.statusBounceTripped(id, "in-review", "human-required") {
			t.Fatal("tripped before the repeated transition limit")
		}
	}
	if !a.statusBounceTripped(id, "human-required", "in-review") {
		t.Fatal("did not trip after repeated reciprocal transitions")
	}
}

func TestStatusBounceTrippedIgnoresOneWayAndBlockedTransitions(t *testing.T) {
	var a App
	for i := 0; i < statusBounceLimit+2; i++ {
		if a.statusBounceTripped("task-one-way", "todo", "in-progress") {
			t.Fatal("one-way transition unexpectedly tripped")
		}
		if a.statusBounceTripped("task-blocked", "blocked", "in-progress") {
			t.Fatal("blocked transition unexpectedly tripped")
		}
	}
}

func TestStatusHookBlocksRepeatedReciprocalLoop(t *testing.T) {
	_, a := setupTaskService(t)
	a.initStatusHook()
	tk, err := a.tasks.Create("looping task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < statusBounceLimit; i++ {
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusHumanRequired, Actor: "test.status-bounce",
			Extra: task.Update{
				Escalation:      task.OperatorDecisionEvidence("test.status_bounce", "test loop"),
				AutonomyOutcome: task.HumanRequiredOutcome(),
			},
			OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to human-required (%d): %v", i, err)
		}
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusInReview, Actor: "test.status-bounce", OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to in-review (%d): %v", i, err)
		}
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusBlocked {
		t.Fatalf("status = %q, want %q after loop circuit breaker", got.Status, task.StatusBlocked)
	}
	if got.Escalation.Code != "workflow.status_bounce" {
		t.Fatalf("escalation = %+v, want workflow.status_bounce", got.Escalation)
	}
}
