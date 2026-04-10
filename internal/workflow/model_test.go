package workflow

import (
	"strings"
	"testing"
)

func TestValidate_MaxRetriesWithinLimit(t *testing.T) {
	d := Definition{
		Steps: []Step{
			{ID: "s1", Config: StepConfig{MaxRetries: 3}},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_MaxRetriesExceedsLimit(t *testing.T) {
	d := Definition{
		Steps: []Step{
			{ID: "s1", Config: StepConfig{MaxRetries: 15}},
		},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for max_retries exceeding limit")
	}
}

func TestValidate_ZeroMaxRetries(t *testing.T) {
	d := Definition{
		Steps: []Step{
			{ID: "s1", Config: StepConfig{MaxRetries: 0}},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_ExactlyAtLimit(t *testing.T) {
	d := Definition{
		Steps: []Step{
			{ID: "s1", Config: StepConfig{MaxRetries: maxRetries}},
		},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStepByID_NotFound(t *testing.T) {
	d := Definition{Steps: []Step{{ID: "a"}}}
	if s := d.StepByID("missing"); s != nil {
		t.Fatalf("expected nil, got %q", s.ID)
	}
}

func TestFirstStep_Empty(t *testing.T) {
	d := Definition{}
	if s := d.FirstStep(); s != nil {
		t.Fatal("expected nil for empty steps")
	}
}

// TestValidateFields_AcceptsKnownAndVars confirms the validator accepts every
// field in KnownTriggerFields plus any "vars.*" prefix. Regression guard so
// adding a caller-side field without updating the registry breaks loudly.
func TestValidateFields_AcceptsKnownAndVars(t *testing.T) {
	// Pick a safe value per field: enum-shaped fields need a value from
	// FieldAllowedValues, free-form fields accept anything.
	safeValue := func(field string) string {
		if allowed, ok := FieldAllowedValues[field]; ok {
			for v := range allowed {
				return v
			}
		}
		return "x"
	}
	for field := range KnownTriggerFields {
		d := Definition{
			ID: "known-" + field,
			Trigger: Trigger{
				Conditions: []Condition{{Field: field, Operator: "equals", Value: safeValue(field)}},
			},
		}
		if err := d.ValidateFields(); err != nil {
			t.Errorf("field %q should be known: %v", field, err)
		}
	}

	d := Definition{
		ID: "vars-prefix",
		Trigger: Trigger{
			Conditions: []Condition{{Field: "vars.human_action", Operator: "equals", Value: "approve"}},
		},
		Steps: []Step{{
			ID:   "s1",
			Next: []Transition{{When: &Condition{Field: "vars.anything_goes", Operator: "equals", Value: "ok"}, GoTo: "end"}},
		}},
	}
	if err := d.ValidateFields(); err != nil {
		t.Errorf("vars.* prefix should be accepted: %v", err)
	}
}

// TestValidateFields_RejectsUnknown is the core dead-YAML regression test.
// The historical bug: auto-merge.yaml referenced "project.type" in a trigger
// condition, but no caller ever populated that field, so the workflow was
// unreachable. ValidateFields must fail for any such reference — at workflow
// save time and for any builtin at test time.
func TestValidateFields_RejectsUnknown(t *testing.T) {
	d := Definition{
		ID: "dead-project-type",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "pr.issue_kind", Operator: "equals", Value: "ready_to_merge"},
				{Field: "project.type", Operator: "equals", Value: "pet"},
			},
		},
	}
	err := d.ValidateFields()
	if err == nil {
		t.Fatal("expected error for unknown field project.type")
	}
	if !strings.Contains(err.Error(), "project.type") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

// TestValidateFields_RejectsUnknownInTransition locks in that transition
// conditions (the "when" block of next[]) are validated too, not just top-
// level trigger conditions. A misspelled field in a mid-workflow branch
// would otherwise silently fall through to the default transition.
func TestValidateFields_RejectsUnknownInTransition(t *testing.T) {
	d := Definition{
		ID: "dead-transition-field",
		Steps: []Step{{
			ID: "fork",
			Next: []Transition{
				{When: &Condition{Field: "task.statuss", Operator: "equals", Value: "done"}, GoTo: "a"},
				{GoTo: "b"},
			},
		}},
	}
	if err := d.ValidateFields(); err == nil {
		t.Fatal("expected error for typo in transition field")
	}
}

// TestValidateFields_RejectsStaleEnumValue is the second half of the
// dispatch-mismatch regression: a condition comparing pr.issue_kind against
// "ci-failure" (dash) instead of "ci_failure" (underscore) must fail
// validation because the constant only ever emits the underscore form.
// Mirrors exactly the shape of the historical bug.
func TestValidateFields_RejectsStaleEnumValue(t *testing.T) {
	d := Definition{
		ID: "stale-enum",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "pr.issue_kind", Operator: "equals", Value: "ci-failure"},
			},
		},
	}
	err := d.ValidateFields()
	if err == nil {
		t.Fatal("expected error for stale ci-failure value")
	}
	if !strings.Contains(err.Error(), "pr.issue_kind") || !strings.Contains(err.Error(), "ci-failure") {
		t.Errorf("error should mention field and bad value, got: %v", err)
	}
}

// TestValidateFields_RejectsInvalidStatus catches task.status comparisons
// against values that don't exist in the Status enum (typo or stale rename).
// The enum source is internal/task/model.go; the lock-step test for that
// cross-file consistency lives in the main package.
func TestValidateFields_RejectsInvalidStatus(t *testing.T) {
	d := Definition{
		ID: "typo-status",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "task.status", Operator: "equals", Value: "in_review"}, // dash vs underscore
			},
		},
	}
	if err := d.ValidateFields(); err == nil {
		t.Fatal("expected error for in_review (should be in-review)")
	}
}

// TestValidateFields_InOperatorChecksEachCSVEntry locks the csv-aware
// validation for the `in`/`not_in` operators: a single bad entry in a list
// of otherwise valid values must still fail.
func TestValidateFields_InOperatorChecksEachCSVEntry(t *testing.T) {
	d := Definition{
		ID: "mixed-csv",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "pr.issue_kind", Operator: "in", Value: "conflict,ci_failure,bogus"},
			},
		},
	}
	err := d.ValidateFields()
	if err == nil {
		t.Fatal("expected error for bogus csv entry")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the bad entry, got: %v", err)
	}
}

// TestValidateFields_ContainsOperatorSkipsEnumCheck ensures substring-style
// operators (`contains`/`not_contains`) on free-form list fields like
// task.tags still pass validation — they have no enum set registered.
func TestValidateFields_ContainsOperatorSkipsEnumCheck(t *testing.T) {
	d := Definition{
		ID: "contains-skip",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "task.tags", Operator: "contains", Value: "review"},
			},
		},
	}
	if err := d.ValidateFields(); err != nil {
		t.Errorf("contains on free-form field should pass: %v", err)
	}
}

// TestValidateFields_RejectsContainsOnEnumField locks the second half of
// the pr-fix drift regression: `operator: contains` on a scalar enum field
// like pr.issue_kind was evaluated as substring matching, and a csv-style
// value like "conflict,ci_failure" silently never matched the actual runtime
// value "ci_failure". ValidateFields must reject the operator so the
// misconfiguration fails loudly at save time instead.
func TestValidateFields_RejectsContainsOnEnumField(t *testing.T) {
	d := Definition{
		ID: "contains-on-enum",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "pr.issue_kind", Operator: "contains", Value: "conflict,ci_failure"},
			},
		},
	}
	err := d.ValidateFields()
	if err == nil {
		t.Fatal("expected error for contains on pr.issue_kind")
	}
	if !strings.Contains(err.Error(), "pr.issue_kind") ||
		!strings.Contains(err.Error(), "contains") {
		t.Errorf("error should mention field and operator, got: %v", err)
	}
}

// TestValidateFields_RejectsNotContainsOnEnumField covers the inverse
// operator — same semantic issue, same rule.
func TestValidateFields_RejectsNotContainsOnEnumField(t *testing.T) {
	d := Definition{
		ID: "not-contains-on-enum",
		Trigger: Trigger{
			Conditions: []Condition{
				{Field: "task.status", Operator: "not_contains", Value: "done"},
			},
		},
	}
	if err := d.ValidateFields(); err == nil {
		t.Fatal("expected error for not_contains on task.status")
	}
}

// TestValidateFields_RejectsUnknownInParallel covers nested steps — a
// parallel sub-step's transitions must be validated recursively.
func TestValidateFields_RejectsUnknownInParallel(t *testing.T) {
	d := Definition{
		ID: "dead-parallel-field",
		Steps: []Step{{
			ID: "group",
			Parallel: []Step{{
				ID:   "sub",
				Next: []Transition{{When: &Condition{Field: "bogus.field", Operator: "equals", Value: "x"}, GoTo: "end"}},
			}},
		}},
	}
	if err := d.ValidateFields(); err == nil {
		t.Fatal("expected error for unknown field in parallel sub-step")
	}
}
