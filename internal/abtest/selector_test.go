package abtest

import (
	"fmt"
	"testing"
)

func TestSelectDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	a, ok, err := Select(cfg, "task-1", "implementation", "implement")
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
	}
	for range 10 {
		got, ok, err := Select(cfg, "task-1", "implementation", "implement")
		if err != nil || !ok {
			t.Fatalf("Select repeat: ok=%v err=%v", ok, err)
		}
		if got != a {
			t.Fatalf("assignment changed: got %+v want %+v", got, a)
		}
	}
}

func TestSelectStageUnitIncludesStep(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "a", Provider: "claude", Model: "opus", Weight: 1},
			{ID: "b", Provider: "codex", Model: "gpt-5.5", Weight: 1},
		},
	}}}
	a, ok, err := Select(cfg, "task", "implementation", "one")
	if err != nil || !ok {
		t.Fatalf("Select one: ok=%v err=%v", ok, err)
	}
	if a.AssignmentKey != "task|implementation|one" {
		t.Fatalf("assignment key = %q", a.AssignmentKey)
	}
}

func TestSelectWeightedDistribution(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "a", Provider: "claude", Model: "opus", Weight: 3},
			{ID: "b", Provider: "codex", Model: "gpt-5.5", Weight: 1},
		},
	}}}
	counts := map[string]int{}
	for i := range 1000 {
		a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), "implementation", "implement")
		if err != nil || !ok {
			t.Fatalf("Select %d: ok=%v err=%v", i, ok, err)
		}
		counts[a.VariantID]++
	}
	if counts["a"] < 650 || counts["a"] > 850 {
		t.Fatalf("weighted distribution off: %+v", counts)
	}
}

func TestSelectSkipsNonMatchingRole(t *testing.T) {
	cfg := DefaultConfig()
	if _, ok, err := Select(cfg, "task-1", "review", "review"); err != nil || ok {
		t.Fatalf("Select review ok=%v err=%v, want disabled", ok, err)
	}
}

func TestSelectValidatesAllPositiveWeightVariants(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "good", Provider: "claude", Model: "opus", Weight: 1},
			{ID: "bad", Provider: "bogus", Model: "x", Weight: 1},
		},
	}}}
	if _, _, err := Select(cfg, "task-1", "implementation", "implement"); err == nil {
		t.Fatal("Select should reject invalid positive-weight variant before hashing")
	}
}

func TestSelectEligibleSkipsUnavailableProvider(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "missing", Provider: "copilot", Model: "gpt-5.5", Weight: 100},
			{ID: "available", Provider: "claude", Model: "opus", Weight: 1},
		},
	}}}
	a, ok, err := SelectEligible(cfg, "task-1", "implementation", "implement", func(provider string) bool {
		return provider == "claude"
	})
	if err != nil || !ok {
		t.Fatalf("SelectEligible ok=%v err=%v", ok, err)
	}
	if a.VariantID != "available" {
		t.Fatalf("VariantID = %q, want available", a.VariantID)
	}
}
