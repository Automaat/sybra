package workflow

import "testing"

func hasCondition(conds []Condition, field, op, value string) bool {
	for _, c := range conds {
		if c.Field == field && c.Operator == op && c.Value == value {
			return true
		}
	}
	return false
}

func defByID(t *testing.T, id string) *Definition {
	t.Helper()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for i := range defs {
		if defs[i].ID == id {
			return &defs[i]
		}
	}
	t.Fatalf("%s builtin definition not found", id)
	return nil
}

// TestBuiltinHandoff_SkipsPlanningToImplement locks the handoff contract: a
// task tagged `handoff` fires simple-task-handoff on creation, which flips it
// straight to in-progress (no triage, no planning) so simple-task-implement
// runs. simple-task-plan must exclude handoff tasks so it does not also fire.
func TestBuiltinHandoff_SkipsPlanningToImplement(t *testing.T) {
	t.Parallel()

	handoff := defByID(t, "simple-task-handoff")
	if handoff.Trigger.On != "task.created" {
		t.Errorf("handoff trigger.on = %q, want task.created", handoff.Trigger.On)
	}
	if !hasCondition(handoff.Trigger.Conditions, "task.tags", "contains", "handoff") {
		t.Errorf("handoff trigger must require tag handoff; got %+v", handoff.Trigger.Conditions)
	}
	first := handoff.FirstStep()
	if first == nil || first.Type != StepSetStatus || first.Config.Status != "in-progress" {
		t.Errorf("handoff first step must set status in-progress; got %+v", first)
	}

	// simple-task-plan must NOT fire for handoff tasks.
	plan := defByID(t, "simple-task-plan")
	if !hasCondition(plan.Trigger.Conditions, "task.tags", "not_contains", "handoff") {
		t.Errorf("simple-task-plan must exclude handoff tasks; got %+v", plan.Trigger.Conditions)
	}
	// The implement-stage handoff must NOT also fire for review- or
	// testing-stage tasks (which also carry the bare `handoff` tag).
	if !hasCondition(handoff.Trigger.Conditions, "task.tags", "not_contains", "handoff-review") {
		t.Errorf("simple-task-handoff must exclude handoff-review tasks; got %+v", handoff.Trigger.Conditions)
	}
	if !hasCondition(handoff.Trigger.Conditions, "task.tags", "not_contains", "handoff-testing") {
		t.Errorf("simple-task-handoff must exclude handoff-testing tasks; got %+v", handoff.Trigger.Conditions)
	}
}

// TestBuiltinHandoffTesting_SkipsToTesting locks the testing-stage contract: a
// task tagged `handoff-testing` fires simple-task-handoff-testing on creation,
// which flips it straight to testing (no implement/review) so testing-task runs
// the adversarial test-runner against the adopted worktree.
func TestBuiltinHandoffTesting_SkipsToTesting(t *testing.T) {
	t.Parallel()

	ht := defByID(t, "simple-task-handoff-testing")
	if ht.Trigger.On != "task.created" {
		t.Errorf("handoff-testing trigger.on = %q, want task.created", ht.Trigger.On)
	}
	if !hasCondition(ht.Trigger.Conditions, "task.tags", "contains", "handoff-testing") {
		t.Errorf("handoff-testing trigger must require tag handoff-testing; got %+v", ht.Trigger.Conditions)
	}
	first := ht.FirstStep()
	if first == nil || first.Type != StepSetStatus || first.Config.Status != "testing" {
		t.Errorf("handoff-testing first step must set status testing; got %+v", first)
	}

	// testing-task enters on status=testing, so the cascade reaches it.
	testWf := defByID(t, "testing-task")
	if !hasCondition(testWf.Trigger.Conditions, "task.status", "equals", "testing") {
		t.Errorf("testing-task must trigger on status=testing; got %+v", testWf.Trigger.Conditions)
	}
}

// TestBuiltinHandoffReview_SkipsToReview locks the review-stage contract: a task
// tagged `handoff-review` fires simple-task-handoff-review on creation, which
// flips it straight to ready-review (no implement) so simple-task-review reviews
// the adopted worktree and opens the PR.
func TestBuiltinHandoffReview_SkipsToReview(t *testing.T) {
	t.Parallel()

	hr := defByID(t, "simple-task-handoff-review")
	if hr.Trigger.On != "task.created" {
		t.Errorf("handoff-review trigger.on = %q, want task.created", hr.Trigger.On)
	}
	if !hasCondition(hr.Trigger.Conditions, "task.tags", "contains", "handoff-review") {
		t.Errorf("handoff-review trigger must require tag handoff-review; got %+v", hr.Trigger.Conditions)
	}
	first := hr.FirstStep()
	if first == nil || first.Type != StepSetStatus || first.Config.Status != "ready-review" {
		t.Errorf("handoff-review first step must set status ready-review; got %+v", first)
	}

	// simple-task-review enters on ready-review, so the cascade reaches it.
	review := defByID(t, "simple-task-review")
	if !hasCondition(review.Trigger.Conditions, "task.status", "equals", "ready-review") {
		t.Errorf("simple-task-review must trigger on status=ready-review; got %+v", review.Trigger.Conditions)
	}
}
