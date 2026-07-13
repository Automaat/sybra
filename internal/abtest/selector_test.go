package abtest

import (
	"fmt"
	"reflect"
	"slices"
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
		if !reflect.DeepEqual(got, a) {
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
	cases := []struct {
		role         string
		experimentID string
		variantIDs   []string
	}{
		{"implementation", "code-author-cheap", []string{"claude-sonnet", "codex-gpt-5.4", "copilot-sonnet"}},
		{"test-runner", "code-author-maintenance-cheap", []string{"claude-sonnet", "codex-gpt-5.4", "copilot-sonnet"}},
		{"pr-fix", "code-author-maintenance-cheap", []string{"claude-sonnet", "codex-gpt-5.4", "copilot-sonnet"}},
	}
	for _, tt := range cases {
		t.Run(tt.role, func(t *testing.T) {
			for i := range 100 {
				a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), tt.role, "step")
				if err != nil || !ok {
					t.Fatalf("Select ok=%v err=%v", ok, err)
				}
				if a.ExperimentID != tt.experimentID {
					t.Fatalf("ExperimentID = %q, want %s", a.ExperimentID, tt.experimentID)
				}
				if !slices.Contains(tt.variantIDs, a.VariantID) {
					t.Fatalf("VariantID = %q, want one of %v", a.VariantID, tt.variantIDs)
				}
			}
		})
	}
}

// TestDefaultConfigEnrollsEveryProviderUniformly locks the "equal agents"
// posture: every default model-comparison experiment enrolls all three
// providers at weight 1 so no role is shut out of any provider and the
// scorecard sees balanced samples. Prompt/skill experiments are exempt — they
// compare variant text on one fixed provider/model (validatePromptSkillHomogeneity
// requires it), and an unenrolled challenger is deliberately weight 0 until its
// offline eval gate passes.
func TestDefaultConfigEnrollsEveryProviderUniformly(t *testing.T) {
	cfg := DefaultConfig()
	for _, exp := range cfg.Experiments {
		if exp.KindValue() != "model" {
			continue
		}
		providers := map[string]bool{}
		for _, v := range exp.Variants {
			if v.Weight != 1 {
				t.Errorf("experiment %q variant %q weight = %d, want 1 (uniform)", exp.ID, v.ID, v.Weight)
			}
			providers[v.Provider] = true
		}
		for _, p := range []string{"claude", "codex", "copilot"} {
			if !providers[p] {
				t.Errorf("experiment %q is missing provider %q", exp.ID, p)
			}
		}
	}
}

func TestDefaultConfigUsesExpensiveBracketForFixReview(t *testing.T) {
	cfg := DefaultConfig()
	for i := range 100 {
		a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), "fix-review", "step")
		if err != nil || !ok {
			t.Fatalf("Select ok=%v err=%v", ok, err)
		}
		if a.ExperimentID != "fix-review-expensive" {
			t.Fatalf("ExperimentID = %q, want fix-review-expensive", a.ExperimentID)
		}
		switch a.VariantID {
		case "claude-opus", "codex-gpt-5.5", "copilot-gemini-3.1-pro":
		default:
			t.Fatalf("VariantID = %q, want an expensive fix-review variant", a.VariantID)
		}
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

func TestSelectEligibleFallsBackWhenAllVariantsShareUnhealthyProvider(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "single-provider-exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "a", Provider: "claude", Model: "opus", Weight: 50},
			{ID: "b", Provider: "claude", Model: "sonnet", Weight: 50},
		},
	}}}
	a, ok, err := SelectEligible(cfg, "task-1", "implementation", "implement", func(provider string) bool {
		return provider != "claude"
	})
	if err != nil {
		t.Fatalf("SelectEligible err = %v, want nil (should defer to normal provider failover)", err)
	}
	if ok {
		t.Fatalf("SelectEligible ok = true, want false so callers fall back to non-AB dispatch; got %+v", a)
	}
}

func TestSelectEligibleStillErrorsWhenAllWeightsAreZero(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "zero-weight-exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "a", Provider: "claude", Model: "opus", Weight: 0},
			{ID: "b", Provider: "codex", Model: "gpt-5.5", Weight: 0},
		},
	}}}
	_, ok, err := SelectEligible(cfg, "task-1", "implementation", "implement", func(provider string) bool {
		return true
	})
	if err == nil {
		t.Fatalf("SelectEligible err = nil, want config error for all-zero-weight experiment")
	}
	if ok {
		t.Fatalf("SelectEligible ok = true, want false")
	}
}

func TestSelectPromptSkillPayload(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "payload",
		Kind:           "compound",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{{
			ID:              "v2",
			Provider:        "codex",
			Model:           "gpt-5.5",
			PromptTransform: &PromptTransform{Op: "append", Text: "\nUse the variant."},
			SkillAliases:    map[string]string{"/sybra-test": "/sybra-test-v2"},
			Weight:          1,
		}},
	}}}
	a, ok, err := Select(cfg, "task-1", "implementation", "implement")
	if err != nil || !ok {
		t.Fatalf("Select ok=%v err=%v", ok, err)
	}
	if a.PromptTransform == nil || a.PromptTransform.Op != "append" || a.PromptTransform.Text != "\nUse the variant." {
		t.Fatalf("PromptTransform = %+v", a.PromptTransform)
	}
	if got := a.SkillAliases["sybra-test"]; got != "sybra-test-v2" {
		t.Fatalf("SkillAliases[sybra-test] = %q", got)
	}
}

func TestValidateVariantPromptTransform(t *testing.T) {
	tests := []struct {
		name    string
		pt      *PromptTransform
		wantErr bool
	}{
		{name: "nil"},
		{name: "empty op", pt: &PromptTransform{}},
		{name: "prepend empty text", pt: &PromptTransform{Op: "prepend"}},
		{name: "append empty text", pt: &PromptTransform{Op: "append"}},
		{name: "replace text", pt: &PromptTransform{Op: "replace", Text: "x"}},
		{name: "template text", pt: &PromptTransform{Op: "template", Text: "x"}},
		{name: "replace empty", pt: &PromptTransform{Op: "replace"}, wantErr: true},
		{name: "template whitespace", pt: &PromptTransform{Op: "template", Text: " \t"}, wantErr: true},
		{name: "unknown", pt: &PromptTransform{Op: "merge", Text: "x"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVariant("exp", Variant{ID: "v", Provider: "claude", Model: "sonnet", PromptTransform: tt.pt})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateVariant err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateVariantSkillAliases(t *testing.T) {
	tests := []struct {
		name    string
		aliases map[string]string
		wantErr bool
	}{
		{name: "nil"},
		{name: "slash and no slash", aliases: map[string]string{"/sybra-test": "sybra-test-v2"}},
		{name: "empty source", aliases: map[string]string{"": "sybra-test-v2"}, wantErr: true},
		{name: "empty target", aliases: map[string]string{"sybra-test": ""}, wantErr: true},
		{name: "whitespace source", aliases: map[string]string{" sybra-test": "sybra-test-v2"}, wantErr: true},
		{name: "path source", aliases: map[string]string{"tmp/sybra-test": "sybra-test-v2"}, wantErr: true},
		{name: "dollar target", aliases: map[string]string{"sybra-test": "$sybra-test-v2"}, wantErr: true},
		{name: "uppercase target", aliases: map[string]string{"sybra-test": "Sybra-Test-V2"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVariant("exp", Variant{ID: "v", Provider: "claude", Model: "sonnet", SkillAliases: tt.aliases})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateVariant err = %v, wantErr %v", err, tt.wantErr)
			}
		})
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

func TestConfigValidatePromptSkillAllowsEmptyReasoningEffortAsDefault(t *testing.T) {
	cfg := Config{Experiments: []Experiment{{
		ID:      "prompt-default-effort",
		Kind:    "prompt",
		Subject: &Subject{StepID: "implement"},
		Variants: []Variant{
			{ID: "explicit-medium", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
			{ID: "omitted-effort", Provider: "claude", Model: "sonnet", ReasoningEffort: "", Weight: 1},
		},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
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

func TestSelectEligibleEvalPassedSeam(t *testing.T) {
	enabled := true
	cfg := Config{Enabled: &enabled, Experiments: []Experiment{{
		ID:             "exp",
		AssignmentUnit: "task",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "failing", Provider: "claude", Model: "opus", Digest: "digest-fail", Weight: 1},
			{ID: "passing", Provider: "claude", Model: "opus", Digest: "digest-pass", Weight: 1},
		},
	}}}
	evalPassed := func(variantID, digest string) bool {
		return variantID == "passing"
	}
	for range 10 {
		a, ok, err := SelectEligibleWithEval(cfg, "task-1", "implementation", "implement", nil, evalPassed)
		if err != nil || !ok {
			t.Fatalf("SelectEligibleWithEval: ok=%v err=%v", ok, err)
		}
		if a.VariantID != "passing" {
			t.Fatalf("VariantID = %q, want passing (failing digest must be excluded)", a.VariantID)
		}
	}
}

func TestSelectEligibleNilEvalPassedUnchanged(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate: %v", err)
	}
	want, ok, err := SelectEligible(cfg, "task-1", "implementation", "implement", nil)
	if err != nil || !ok {
		t.Fatalf("SelectEligible: ok=%v err=%v", ok, err)
	}
	got, ok, err := SelectEligibleWithEval(cfg, "task-1", "implementation", "implement", nil, nil)
	if err != nil || !ok {
		t.Fatalf("SelectEligibleWithEval(evalPassed=nil): ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("evalPassed=nil changed selection: got %+v want %+v", got, want)
	}
}

func TestEligibleVariantsSkipsOnlyDigestedFailures(t *testing.T) {
	exp := Experiment{Variants: []Variant{
		{ID: "no-digest", Provider: "claude", Model: "opus", Weight: 1}, // model-only, no digest
		{ID: "has-digest-fail", Provider: "claude", Model: "opus", Digest: "d1", Weight: 1},
		{ID: "has-digest-pass", Provider: "claude", Model: "opus", Digest: "d2", Weight: 1},
	}}
	evalPassed := func(variantID, digest string) bool { return digest == "d2" }
	eligible, total := EligibleVariants(exp, nil, evalPassed)
	if total != 2 {
		t.Fatalf("total = %d, want 2 (digest-less variant + the passing digested one)", total)
	}
	ids := map[string]bool{}
	for _, v := range eligible {
		ids[v.ID] = true
	}
	if !ids["no-digest"] || !ids["has-digest-pass"] || ids["has-digest-fail"] {
		t.Fatalf("eligible = %+v", eligible)
	}
}
