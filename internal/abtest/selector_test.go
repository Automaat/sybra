package abtest

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSelectDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate: %v", err)
	}
	a, ok, err := Select(cfg, "task-1", "implementation", "implement")
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
	}
	if a.Kind != "model" {
		t.Fatalf("Kind = %q, want model", a.Kind)
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

func TestConfigValidateChecksDisabledExperiments(t *testing.T) {
	disabled := false
	cfg := Config{Experiments: []Experiment{{
		ID:      "disabled-invalid",
		Enabled: &disabled,
		Kind:    "bogus",
		Variants: []Variant{
			{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1},
		},
	}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate should reject disabled invalid experiment")
	}
}

func TestConfigValidatePromptAndSkillYAML(t *testing.T) {
	raw := []byte(`
enabled: true
experiments:
  - id: prompt-copy
    kind: prompt
    assignment_unit: stage
    subject:
      workflow_id: test-workflow
      step_id: draft_prompt
      role: implementation
    variants:
      - id: control
        provider: claude
        model: sonnet
        reasoning_effort: medium
        version: v1
        digest: sha256:control
        weight: 1
      - id: treatment
        provider: claude
        model: sonnet
        reasoning_effort: medium
        version: v2
        digest: sha256:treatment
        weight: 1
  - id: skill-choice
    kind: skill
    assignment_unit: task
    subject:
      role: review
      skill_name: sybra-test
    variants:
      - id: control
        provider: codex
        model: gpt-5.5
        weight: 1
      - id: treatment
        provider: codex
        model: gpt-5.5
        version: v2
        digest: sha256:skill
        weight: 1
`)
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Experiments[0].Subject.WorkflowID; got != "test-workflow" {
		t.Fatalf("workflow_id = %q", got)
	}
	if got := cfg.Experiments[0].Variants[1].Version; got != "v2" {
		t.Fatalf("version = %q", got)
	}
	if got := cfg.Experiments[1].Subject.SkillName; got != "sybra-test" {
		t.Fatalf("skill_name = %q", got)
	}
	if got := cfg.Experiments[1].Variants[1].Digest; got != "sha256:skill" {
		t.Fatalf("digest = %q", got)
	}
}

func TestConfigValidatePromptAndSkillAllowWorkflowOnlySubject(t *testing.T) {
	for _, kind := range []string{"prompt", "skill"} {
		t.Run(kind, func(t *testing.T) {
			subject := &Subject{WorkflowID: "simple-task"}
			if kind == "skill" {
				subject.SkillName = "sybra-test"
			}
			cfg := Config{Experiments: []Experiment{{
				ID:      kind + "-workflow-only",
				Kind:    kind,
				Subject: subject,
				Variants: []Variant{
					{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1},
				},
			}}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestConfigValidatePromptSkillRejectsProviderModelReasoningDrift(t *testing.T) {
	tests := []struct {
		name       string
		kind       string
		variant    Variant
		wantFields []string
	}{
		{
			name:    "prompt provider",
			kind:    "prompt",
			variant: Variant{ID: "bad-provider", Provider: "codex", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
			wantFields: []string{
				`experiment "prompt-provider"`,
				"provider",
				`variant "bad-provider"`,
			},
		},
		{
			name:    "prompt model",
			kind:    "prompt",
			variant: Variant{ID: "bad-model", Provider: "claude", Model: "opus", ReasoningEffort: "medium", Weight: 1},
			wantFields: []string{
				`experiment "prompt-model"`,
				"model",
				`variant "bad-model"`,
			},
		},
		{
			name:    "skill reasoning",
			kind:    "skill",
			variant: Variant{ID: "bad-reasoning", Provider: "claude", Model: "sonnet", ReasoningEffort: "high", Weight: 1},
			wantFields: []string{
				`experiment "skill-reasoning"`,
				"reasoning_effort",
				`variant "bad-reasoning"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expID := strings.ReplaceAll(tt.name, " ", "-")
			subject := &Subject{StepID: "implement"}
			if tt.kind == "skill" {
				subject.SkillName = "sybra-test"
			}
			cfg := Config{Experiments: []Experiment{{
				ID:      expID,
				Kind:    tt.kind,
				Subject: subject,
				Variants: []Variant{
					{ID: "base", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
					tt.variant,
				},
			}}}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate should reject prompt/skill model attribution drift")
			}
			msg := err.Error()
			for _, want := range tt.wantFields {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q does not contain %q", msg, want)
				}
			}
		})
	}
}

func TestConfigValidateCompoundAllowsProviderModelReasoningDrift(t *testing.T) {
	cfg := Config{Experiments: []Experiment{{
		ID:   "compound",
		Kind: "compound",
		Variants: []Variant{
			{ID: "claude", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
			{ID: "codex", Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high", Weight: 1},
		},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestConfigValidateRejectsInvalidKindAndAssignmentUnit(t *testing.T) {
	tests := []struct {
		name string
		exp  Experiment
	}{
		{
			name: "empty id",
			exp: Experiment{
				Variants: []Variant{{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1}},
			},
		},
		{
			name: "invalid kind",
			exp: Experiment{
				ID:       "bad-kind",
				Kind:     "Prompt",
				Variants: []Variant{{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1}},
			},
		},
		{
			name: "invalid assignment_unit",
			exp: Experiment{
				ID:             "bad-unit",
				AssignmentUnit: "workflow",
				Variants:       []Variant{{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := (Config{Experiments: []Experiment{tt.exp}}).Validate(); err == nil {
				t.Fatal("Validate should reject invalid experiment")
			}
		})
	}
}

func TestConfigValidateRejectsMissingPromptSkillSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject *Subject
	}{
		{name: "missing", subject: nil},
		{name: "whitespace", subject: &Subject{WorkflowID: "  ", StepID: " \t", Role: "  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Experiments: []Experiment{{
				ID:      "prompt-subject-" + tt.name,
				Kind:    "prompt",
				Subject: tt.subject,
				Variants: []Variant{
					{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1},
				},
			}}}
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate should reject missing prompt subject")
			}
		})
	}
}

func TestConfigValidateRejectsMissingSkillName(t *testing.T) {
	cfg := Config{Experiments: []Experiment{{
		ID:      "skill-no-name",
		Kind:    "skill",
		Subject: &Subject{StepID: "implement"},
		Variants: []Variant{
			{ID: "v", Provider: "claude", Model: "sonnet", Weight: 1},
		},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate should reject skill experiment without subject skill_name")
	}
	if !strings.Contains(err.Error(), "skill_name") {
		t.Fatalf("error %q does not contain %q", err.Error(), "skill_name")
	}
}

func TestSelectIgnoresProviderDriftOnIneligibleVariant(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "prompt-mixed-provider",
		Kind:           "prompt",
		Enabled:        &enabled,
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Subject:        &Subject{StepID: "implement"},
		Variants: []Variant{
			{ID: "claude-variant", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
			{ID: "codex-variant", Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "medium", Weight: 1},
		},
	}}}
	_, ok, err := SelectEligible(cfg, "task-1", "implementation", "implement", func(provider string) bool {
		return provider == "claude"
	})
	if err != nil {
		t.Fatalf("SelectEligible should not fail homogeneity check when the drifting variant is ineligible: %v", err)
	}
	if !ok {
		t.Fatal("SelectEligible ok = false, want true")
	}
}

func TestConfigValidateIgnoresZeroWeightPromptSkillDrift(t *testing.T) {
	for _, kind := range []string{"prompt", "skill"} {
		t.Run(kind, func(t *testing.T) {
			subject := &Subject{Role: "implementation"}
			if kind == "skill" {
				subject.SkillName = "sybra-test"
			}
			cfg := Config{Experiments: []Experiment{{
				ID:      kind + "-zero-weight",
				Kind:    kind,
				Subject: subject,
				Variants: []Variant{
					{ID: "base", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
					{ID: "ignored", Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high", Weight: 0},
				},
			}}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestConfigValidateDuplicateVariantIDOnlyChecksPositiveWeight(t *testing.T) {
	cfg := Config{Experiments: []Experiment{{
		ID: "duplicates",
		Variants: []Variant{
			{ID: "same", Provider: "claude", Model: "sonnet", Weight: 1},
			{ID: "same", Provider: "codex", Model: "gpt-5.5", Weight: 0},
		},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cfg.Experiments[0].Variants[1].Weight = 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate should reject duplicate positive-weight variant ids")
	}
}
