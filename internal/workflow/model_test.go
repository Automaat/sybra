package workflow

import "testing"

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

func TestValidate_MaxRetriesExceedsInParallel(t *testing.T) {
	d := Definition{
		Steps: []Step{
			{ID: "s1", Parallel: []Step{
				{ID: "p1", Config: StepConfig{MaxRetries: 20}},
			}},
		},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected error for max_retries in parallel step")
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

func TestStepByID_InParallel(t *testing.T) {
	d := Definition{
		Steps: []Step{{ID: "a", Parallel: []Step{{ID: "b"}}}},
	}
	if s := d.StepByID("b"); s == nil {
		t.Fatal("expected to find parallel step 'b'")
	}
}

func TestFirstStep_Empty(t *testing.T) {
	d := Definition{}
	if s := d.FirstStep(); s != nil {
		t.Fatal("expected nil for empty steps")
	}
}
