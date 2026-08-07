package workflow

import (
	"testing"
	"time"
)

func TestEffectIDComparison(t *testing.T) {
	t.Parallel()

	zero := EffectID{}
	first := EffectID{Generation: 2, StepSeq: 3, StepID: "plan", Pos: 0}
	second := EffectID{Generation: 2, StepSeq: 3, StepID: "plan", Pos: 1}
	laterStep := EffectID{Generation: 2, StepSeq: 4, StepID: "review", Pos: 0}
	nextGen := EffectID{Generation: 3, StepSeq: 0, StepID: "start", Pos: 0}

	if !zero.IsZero() {
		t.Fatal("zero EffectID must report IsZero")
	}
	if first.IsZero() {
		t.Fatal("assigned EffectID reported zero")
	}
	if zero.String() != "effect:zero" {
		t.Fatalf("zero.String() = %q, want effect:zero", zero.String())
	}
	if first.String() != "g2:s3:plan:0" {
		t.Fatalf("first.String() = %q, want g2:s3:plan:0", first.String())
	}
	firstCopy := first
	if !first.Equal(firstCopy) {
		t.Fatal("Equal must match identical ids")
	}
	if first.Equal(second) {
		t.Fatal("Equal matched distinct ids")
	}
	if !first.Less(second) || !first.Before(second) {
		t.Fatal("position must order sibling effects")
	}
	if !second.Less(laterStep) {
		t.Fatal("later step must sort after earlier sibling")
	}
	if !laterStep.Less(nextGen) {
		t.Fatal("next generation must sort after prior generation")
	}
	nextGenCopy := nextGen
	if got := nextGen.Compare(nextGenCopy); got != 0 {
		t.Fatalf("Compare(self) = %d, want 0", got)
	}
}

func TestReduceAssignsDistinctEffectIDs(t *testing.T) {
	t.Parallel()

	effects, err := Reduce(
		DesiredState{
			Definition: Definition{ID: "wf", Steps: []Step{{ID: "start", Type: StepRunAgent}}},
			WorkflowID: "wf",
		},
		ObservedState{
			Task: TaskInfo{ID: "t1", Generation: 7},
			Now:  time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if len(effects) != 2 {
		t.Fatalf("len(effects) = %d, want 2", len(effects))
	}
	if effects[0].ID.IsZero() || effects[1].ID.IsZero() {
		t.Fatalf("assigned ids must be non-zero: %#v", effects)
	}
	if effects[0].ID.Equal(effects[1].ID) {
		t.Fatalf("effects share id %v", effects[0].ID)
	}
	if !effects[0].ID.Less(effects[1].ID) {
		t.Fatalf("ids not ordered: %v !< %v", effects[0].ID, effects[1].ID)
	}
	if effects[0].ID.Generation != 7 || effects[1].ID.Generation != 7 {
		t.Fatalf("generation = %d/%d, want 7/7", effects[0].ID.Generation, effects[1].ID.Generation)
	}
	if effects[0].ID.StepID != "start" || effects[1].ID.StepID != "start" {
		t.Fatalf("step ids = %q/%q, want start/start", effects[0].ID.StepID, effects[1].ID.StepID)
	}
}

func TestReduceOrdersEffectIDsAcrossSteps(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	effects, err := Reduce(
		DesiredState{
			Definition: Definition{ID: "wf", Steps: []Step{
				{ID: "mark-review", Type: StepSetStatus, Config: StepConfig{Status: "in-review", StatusReason: "ready"}, Next: []Transition{{GoTo: "await"}}},
				{ID: "await", Type: StepWaitHuman, Config: StepConfig{Status: "plan-review", HumanActions: []string{"approve"}}},
			}},
			WorkflowID: "wf",
		},
		ObservedState{
			Task: TaskInfo{ID: "t1", Generation: 3, Status: "todo"},
			Now:  now,
			Execution: &Execution{
				WorkflowID:  "wf",
				CurrentStep: "mark-review",
				State:       ExecRunning,
				StartedAt:   now.Add(-time.Minute),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	requireKinds(t, effects, EffectSetTaskStatus, EffectRecordStep, EffectSetTaskStatus, EffectSetWorkflowState, EffectWaitHuman)
	if got := effects[1].ID.StepID; got != "mark-review" {
		t.Fatalf("record step id = %q, want mark-review", got)
	}
	if got := effects[4].ID.StepID; got != "await" {
		t.Fatalf("wait step id = %q, want await", got)
	}
	if !effects[1].ID.Less(effects[4].ID) {
		t.Fatalf("earlier step id %v must sort before later step id %v", effects[1].ID, effects[4].ID)
	}
	if effects[1].ID.StepSeq >= effects[4].ID.StepSeq {
		t.Fatalf("step seq = %d/%d, want earlier < later", effects[1].ID.StepSeq, effects[4].ID.StepSeq)
	}
}
