package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestStatusBounceTrippedRequiresRepeatedReciprocalTransitions(t *testing.T) {
	var a App
	const id = "task-bounce"

	for range statusBounceLimit - 1 {
		if a.statusBounceTripped(id, string(task.StatusHumanRequired), string(task.StatusInReview)) {
			t.Fatal("tripped before both directions repeated")
		}
		if a.statusBounceTripped(id, string(task.StatusInReview), string(task.StatusHumanRequired)) {
			t.Fatal("tripped before the repeated transition limit")
		}
	}
	if !a.statusBounceTripped(id, string(task.StatusHumanRequired), string(task.StatusInReview)) {
		t.Fatal("did not trip after repeated reciprocal transitions")
	}
}

func TestStatusBounceTrippedIgnoresOneWayAndBlockedTransitions(t *testing.T) {
	var a App
	for range statusBounceLimit + 2 {
		if a.statusBounceTripped("task-one-way", string(task.StatusTodo), string(task.StatusInProgress)) {
			t.Fatal("one-way transition unexpectedly tripped")
		}
		if a.statusBounceTripped("task-blocked", string(task.StatusBlocked), string(task.StatusInProgress)) {
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
	for range statusBounceLimit {
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusHumanRequired, Actor: "test.status-bounce",
			Extra: task.Update{
				Escalation:      task.OperatorDecisionEvidence("test.status_bounce", "test loop"),
				AutonomyOutcome: task.HumanRequiredOutcome(),
			},
			OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to human-required: %v", err)
		}
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusInReview, Actor: "test.status-bounce", OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to in-review: %v", err)
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
