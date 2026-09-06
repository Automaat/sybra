package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestStatusBounceIncidentReopenStartsNewEpisode(t *testing.T) {
	var a App
	now := time.Now()
	for range statusBounceLimit + 2 {
		if a.statusBounceTrippedAt("task-incident", "todo", "done", "workflow", now) {
			t.Fatal("incident completion quarantined")
		}
		if a.statusBounceTrippedAt("task-incident", "done", "todo", "monitor.incident.reopen", now) {
			t.Fatal("incident recurrence quarantined")
		}
	}
	for i := range statusBounceLimit {
		tripped := a.statusBounceTrippedAt("task-incident", "in-review", "in-progress", "fixer", now)
		tripped = a.statusBounceTrippedAt("task-incident", "in-progress", "in-review", "reviewer", now) || tripped
		if i == statusBounceLimit-1 && !tripped {
			t.Fatal("real contention in fresh incident episode escaped")
		}
	}
}

func TestStatusBounceTrippedRequiresRepeatedReciprocalTransitions(t *testing.T) {
	var a App
	const id = "task-bounce"

	base := time.Now()
	for i := range statusBounceLimit - 1 {
		if a.statusBounceTrippedAt(id, string(task.StatusHumanRequired), string(task.StatusInReview), "pr-monitor", base.Add(time.Duration(i)*time.Minute)) {
			t.Fatal("tripped before both directions repeated")
		}
		if a.statusBounceTrippedAt(id, string(task.StatusInReview), string(task.StatusHumanRequired), "review-reconciler", base.Add(time.Duration(i)*time.Minute+30*time.Second)) {
			t.Fatal("tripped before the repeated transition limit")
		}
	}
	if !a.statusBounceTrippedAt(id, string(task.StatusHumanRequired), string(task.StatusInReview), "pr-monitor", base.Add(time.Hour)) {
		t.Fatal("did not trip after repeated reciprocal transitions")
	}
}

func TestStatusBounceTrippedIgnoresOneWayAndBlockedTransitions(t *testing.T) {
	var a App
	for range statusBounceLimit + 2 {
		if a.statusBounceTrippedAt("task-one-way", string(task.StatusTodo), string(task.StatusInProgress), "a", time.Now()) {
			t.Fatal("one-way transition unexpectedly tripped")
		}
		if a.statusBounceTrippedAt("task-blocked", string(task.StatusBlocked), string(task.StatusInProgress), "a", time.Now()) {
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
			TaskID: tk.ID, ToStatus: task.StatusHumanRequired, Actor: "test.escalator",
			Extra: task.Update{
				Escalation:      task.OperatorDecisionEvidence("test.status_bounce", "test loop"),
				AutonomyOutcome: task.HumanRequiredOutcome(),
			},
			OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to human-required: %v", err)
		}
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusInReview, Actor: "test.reviver", OperatorOverride: true,
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

func TestStatusBounceIgnoresOneAutomationDrivingRoundAfterRound(t *testing.T) {
	var a App
	const id = "task-sequential"
	base := time.Now()

	for round := range statusBounceLimit + 2 {
		at := base.Add(time.Duration(round) * 30 * time.Minute)
		if a.statusBounceTrippedAt(id, string(task.StatusInReview), string(task.StatusInProgress), "pr-monitor", at) {
			t.Fatalf("round %d: a task working through one fix round after another was paused as a loop", round+1)
		}
		if a.statusBounceTrippedAt(id, string(task.StatusInProgress), string(task.StatusInReview), "pr-monitor", at.Add(9*time.Minute)) {
			t.Fatalf("round %d: a fix round finishing normally was paused as a loop", round+1)
		}
	}
}

func TestStatusBounceCatchesTwoAutomationsContending(t *testing.T) {
	var a App
	const id = "task-contended"
	base := time.Now()
	tripped := false

	for i := range statusBounceLimit {
		at := base.Add(time.Duration(i) * 20 * time.Second)
		tripped = a.statusBounceTrippedAt(id, string(task.StatusInReview), string(task.StatusInProgress), "pr-monitor", at) || tripped
		tripped = a.statusBounceTrippedAt(id, string(task.StatusInProgress), string(task.StatusInReview), "review-reconciler", at.Add(10*time.Second)) || tripped
	}
	if !tripped {
		t.Fatal("two automations rewriting one task within seconds of each other were not caught")
	}
}

func TestStatusBounceIgnoresSlowRoundsFromOneAutomation(t *testing.T) {
	var a App
	const id = "task-slow-rounds"
	base := time.Now()

	for round := range statusBounceLimit + 3 {
		at := base.Add(time.Duration(round) * 90 * time.Second)
		if a.statusBounceTrippedAt(id, string(task.StatusInReview), string(task.StatusInProgress), "pr-monitor", at) {
			t.Fatalf("round %d: rounds driven by one automation were paused as a loop", round+1)
		}
		if a.statusBounceTrippedAt(id, string(task.StatusInProgress), string(task.StatusInReview), "pr-monitor", at.Add(40*time.Second)) {
			t.Fatalf("round %d: a round finishing normally was paused as a loop", round+1)
		}
	}
}

func TestStatusBounceCatchesContentionSlowerThanAnyWindow(t *testing.T) {
	var a App
	const id = "task-slow-contention"
	base := time.Now()
	tripped := false

	for i := range statusBounceLimit {
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		tripped = a.statusBounceTrippedAt(id, string(task.StatusInReview), string(task.StatusInProgress), "pr-monitor", at) || tripped
		tripped = a.statusBounceTrippedAt(id, string(task.StatusInProgress), string(task.StatusInReview), "review-reconciler", at.Add(5*time.Minute)) || tripped
	}
	if !tripped {
		t.Fatal("two automations ping-ponging on a ten-minute tick escaped the detector")
	}
}

func TestStatusBounceForgetsATaskItStoppedTracking(t *testing.T) {
	var a App
	const id = "task-forgotten"
	base := time.Now()

	a.statusBounceTrippedAt(id, string(task.StatusInReview), string(task.StatusInProgress), "pr-monitor", base)
	a.statusBounceTrippedAt(id, string(task.StatusInProgress), string(task.StatusInReview), "pr-monitor", base.Add(statusBounceWindow+time.Hour))

	a.statusBounceMu.Lock()
	defer a.statusBounceMu.Unlock()
	state, tracked := a.statusBounces[id]
	if !tracked {
		return
	}
	for edge, edges := range state.edges {
		if len(edges) > 1 {
			t.Fatalf("edge %q kept %d entries, want the aged-out ones dropped", edge, len(edges))
		}
	}
}

func TestStatusHookLeavesOneAutomationsRoundsAlone(t *testing.T) {
	_, a := setupTaskService(t)
	a.initStatusHook()
	tk, err := a.tasks.Create("task doing rounds", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	for range statusBounceLimit + 2 {
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusInProgress, Actor: "test.pr-monitor", OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to in-progress: %v", err)
		}
		if _, err := a.tasks.Apply(task.TransitionIntent{
			TaskID: tk.ID, ToStatus: task.StatusInReview, Actor: "test.pr-monitor", OperatorOverride: true,
		}); err != nil {
			t.Fatalf("transition to in-review: %v", err)
		}
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusBlocked {
		t.Fatalf("status = %q, want a task driven by one automation left running: %s", got.Status, got.StatusReason)
	}
}
