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
	if _, ok, err := Select(cfg, "task-1", "deploy", "deploy"); err != nil || ok {
		t.Fatalf("Select deploy ok=%v err=%v, want disabled", ok, err)
	}
}

func TestDefaultConfigUsesCheapBracketForCodeAuthorRoles(t *testing.T) {
	cfg := DefaultConfig()
	for _, role := range []string{"implementation", "test-runner", "fix-review", "pr-fix"} {
		t.Run(role, func(t *testing.T) {
			for i := range 100 {
				a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), role, "step")
				if err != nil || !ok {
					t.Fatalf("Select ok=%v err=%v", ok, err)
				}
				if a.ExperimentID != "code-author-cheap" {
					t.Fatalf("ExperimentID = %q, want code-author-cheap", a.ExperimentID)
				}
				switch a.VariantID {
				case "claude-sonnet", "codex-gpt-5.4", "copilot-sonnet":
				default:
					t.Fatalf("VariantID = %q, want cheap variant", a.VariantID)
				}
			}
		})
	}
}

func TestDefaultConfigUsesExpensiveBracketForReviewRoles(t *testing.T) {
	cfg := DefaultConfig()
	for _, role := range []string{"review", "plan"} {
		t.Run(role, func(t *testing.T) {
			for i := range 100 {
				a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), role, "step")
				if err != nil || !ok {
					t.Fatalf("Select ok=%v err=%v", ok, err)
				}
				if a.ExperimentID != "review-expensive" {
					t.Fatalf("ExperimentID = %q, want review-expensive", a.ExperimentID)
				}
				switch a.VariantID {
				case "claude-opus", "codex-gpt-5.5", "copilot-gemini-3.1-pro":
				default:
					t.Fatalf("VariantID = %q, want expensive variant", a.VariantID)
				}
			}
		})
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

func TestSelectRejectsBracketTierMismatch(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "task",
		Bracket:        "cheap",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "sonnet", Provider: "claude", Model: "sonnet", Tier: "cheap", Weight: 1},
			{ID: "opus", Provider: "claude", Model: "opus", Tier: "expensive", Weight: 1},
		},
	}}}
	if _, _, err := Select(cfg, "task-1", "implementation", "implement"); err == nil {
		t.Fatal("Select should reject expensive variant in cheap bracket")
	}
}

func TestSelectAllowsUnbracketedMixedTiers(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "sonnet", Provider: "claude", Model: "sonnet", Tier: "cheap", Weight: 1},
			{ID: "opus", Provider: "claude", Model: "opus", Tier: "expensive", Weight: 1},
		},
	}}}
	if _, ok, err := Select(cfg, "task-1", "implementation", "implement"); err != nil || !ok {
		t.Fatalf("Select ok=%v err=%v, want unbracketed mixed tiers allowed", ok, err)
	}
}

func TestSelectRejectsInvalidTierAndBracket(t *testing.T) {
	tests := []struct {
		name string
		exp  Experiment
	}{
		{
			name: "invalid tier",
			exp: Experiment{
				ID:             "exp",
				AssignmentUnit: "task",
				Roles:          []string{"implementation"},
				Variants: []Variant{
					{ID: "bad", Provider: "claude", Model: "sonnet", Tier: "budget", Weight: 1},
				},
			},
		},
		{
			name: "invalid bracket",
			exp: Experiment{
				ID:             "exp",
				AssignmentUnit: "task",
				Bracket:        "budget",
				Roles:          []string{"implementation"},
				Variants: []Variant{
					{ID: "sonnet", Provider: "claude", Model: "sonnet", Tier: "cheap", Weight: 1},
				},
			},
		},
	}
	enabled := true
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Enabled: &enabled, Experiments: []Experiment{tt.exp}}
			if _, _, err := Select(cfg, "task-1", "implementation", "implement"); err == nil {
				t.Fatal("Select should reject invalid tier/bracket config")
			}
		})
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
