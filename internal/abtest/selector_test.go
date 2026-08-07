package abtest

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func enabledDefaultConfig() Config {
	cfg := DefaultConfig()
	enabled := true
	cfg.Enabled = &enabled
	return cfg
}

func TestSelectDeterministic(t *testing.T) {
	cfg := enabledDefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate: %v", err)
		panic("unreachable")
	}
	a, ok, err := Select(cfg, "task-1", "implementation", "implement")
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if a.Kind != "model" {
		t.Fatalf("Kind = %q, want model", a.Kind)
	}
	for range 10 {
		got, ok, err := Select(cfg, "task-1", "implementation", "implement")
		if err != nil || !ok {
			t.Fatalf("Select repeat: ok=%v err=%v", ok, err)
			panic("unreachable")
		}
		if !reflect.DeepEqual(got, a) {
			t.Fatalf("assignment changed: got %+v want %+v", got, a)
		}
	}
}

func TestSelectStampsDecisionVersionFromConfigWeightsVersion(t *testing.T) {
	cfg := enabledDefaultConfig()
	a, ok, err := Select(cfg, "task-1", "implementation", "implement")
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if a.DecisionVersion != 0 {
		t.Fatalf("DecisionVersion = %d, want 0 for a config with no WeightsVersion", a.DecisionVersion)
	}

	version := 42
	cfg.WeightsVersion = &version
	a, ok, err = Select(cfg, "task-1", "implementation", "implement")
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if a.DecisionVersion != 42 {
		t.Fatalf("DecisionVersion = %d, want 42", a.DecisionVersion)
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
		panic("unreachable")
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
			panic("unreachable")
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
		panic("unreachable")
	}
}

func TestDefaultConfigUsesCheapBracketForCodeAuthorRoles(t *testing.T) {
	cfg := enabledDefaultConfig()
	cases := []struct {
		role         string
		experimentID string
		variantIDs   []string
	}{
		{"implementation", "code-author-cheap", []string{"claude-sonnet", "codex-gpt-5.4", "copilot-sonnet", "opencode-deepseek-v4-flash"}},
		{"test-runner", "code-author-maintenance-cheap", []string{"claude-sonnet", "codex-gpt-5.4", "copilot-sonnet", "opencode-deepseek-v4-flash"}},
		{"pr-fix", "code-author-maintenance-cheap", []string{"claude-sonnet", "codex-gpt-5.4", "copilot-sonnet", "opencode-deepseek-v4-flash"}},
	}
	for _, tt := range cases {
		t.Run(tt.role, func(t *testing.T) {
			for i := range 100 {
				a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), tt.role, "step")
				if err != nil || !ok {
					t.Fatalf("Select ok=%v err=%v", ok, err)
					panic("unreachable")
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
// posture: every default experiment enrolls every supported provider at weight
// 1 so no role is shut out of any provider and the scorecard sees balanced
// samples.
func TestDefaultConfigEnrollsEveryProviderUniformly(t *testing.T) {
	cfg := enabledDefaultConfig()
	for _, exp := range cfg.Experiments {
		providers := map[string]bool{}
		for _, v := range exp.Variants {
			if v.Weight != 1 {
				t.Errorf("experiment %q variant %q weight = %d, want 1 (uniform)", exp.ID, v.ID, v.Weight)
			}
			providers[v.Provider] = true
		}
		for _, p := range []string{"claude", "codex", "copilot", "opencode"} {
			if !providers[p] {
				t.Errorf("experiment %q is missing provider %q", exp.ID, p)
			}
		}
	}
}

func TestDefaultConfigUsesExpensiveBracketForFixReview(t *testing.T) {
	cfg := enabledDefaultConfig()
	for i := range 100 {
		a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), "fix-review", "step")
		if err != nil || !ok {
			t.Fatalf("Select ok=%v err=%v", ok, err)
			panic("unreachable")
		}
		if a.ExperimentID != "fix-review-expensive" {
			t.Fatalf("ExperimentID = %q, want fix-review-expensive", a.ExperimentID)
		}
		switch a.VariantID {
		case "claude-opus", "codex-gpt-5.5", "copilot-gemini-3.1-pro", "opencode-glm-5.2":
		default:
			t.Fatalf("VariantID = %q, want an expensive fix-review variant", a.VariantID)
		}
	}
}

func TestDefaultConfigUsesExpensiveBracketForReviewRoles(t *testing.T) {
	cfg := enabledDefaultConfig()
	for _, role := range []string{"review", "plan"} {
		t.Run(role, func(t *testing.T) {
			for i := range 100 {
				a, ok, err := Select(cfg, fmt.Sprintf("task-%d", i), role, "step")
				if err != nil || !ok {
					t.Fatalf("Select ok=%v err=%v", ok, err)
					panic("unreachable")
				}
				wantExp := "review-expensive"
				if role == "review" {
					wantExp = "review-tighten-instructions-pl-a2d853b2c1d9"
				}
				if a.ExperimentID != wantExp {
					t.Fatalf("ExperimentID = %q, want %s", a.ExperimentID, wantExp)
				}
				switch a.VariantID {
				case "claude-opus", "codex-gpt-5.5", "copilot-gemini-3.1-pro", "opencode-glm-5.2", "pl-a2d853b2c1d9-codex-gpt-5.5":
				default:
					t.Fatalf("VariantID = %q, want expensive variant", a.VariantID)
				}
			}
		})
	}
}

func TestDefaultConfigReviewTightenVariantIsDigestedPromptTransform(t *testing.T) {
	cfg := enabledDefaultConfig()
	var found *Variant
	for i := range cfg.Experiments {
		if cfg.Experiments[i].ID != "review-tighten-instructions-pl-a2d853b2c1d9" {
			continue
		}
		if cfg.Experiments[i].KindValue() != "compound" {
			t.Fatalf("Kind = %q, want compound", cfg.Experiments[i].KindValue())
		}
		if cfg.Experiments[i].Subject == nil || cfg.Experiments[i].Subject.Role != "review" {
			t.Fatalf("Subject = %+v, want role review", cfg.Experiments[i].Subject)
			panic("unreachable")
		}
		for j := range cfg.Experiments[i].Variants {
			if cfg.Experiments[i].Variants[j].ID == "pl-a2d853b2c1d9-codex-gpt-5.5" {
				found = &cfg.Experiments[i].Variants[j]
			}
		}
	}
	if found == nil {
		t.Fatal("prompt-lab review variant not found")
		panic("unreachable")
	}
	if found.Version != "pl-a2d853b2c1d9" {
		t.Fatalf("Version = %q, want proposal id", found.Version)
	}
	if found.PromptTransform == nil || found.PromptTransform.Op != "append" || found.PromptTransform.Text != ReviewTightenInstructionsPLA2D853B2C1D9 {
		t.Fatalf("PromptTransform = %+v, want append of reviewed text", found.PromptTransform)
		panic("unreachable")
	}
	if want := digestString(ReviewTightenInstructionsPLA2D853B2C1D9); found.Digest != want {
		t.Fatalf("Digest = %q, want %q", found.Digest, want)
	}

	data, err := os.ReadFile("../prompteval/testdata/promptlab-review-tighten-codex-variants.json")
	if err != nil {
		t.Fatalf("read offline fixture: %v", err)
		panic("unreachable")
	}
	var fixtures []struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatalf("parse offline fixture: %v", err)
		panic("unreachable")
	}
	if len(fixtures) != 1 {
		t.Fatalf("offline fixture count = %d, want 1", len(fixtures))
	}
	if fixtures[0].ID != found.ID || fixtures[0].Digest != found.Digest || fixtures[0].Prompt != found.PromptTransform.Text {
		t.Fatalf("offline fixture = %+v, want id/digest/prompt from default variant", fixtures[0])
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
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
				panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
	}
	if a.PromptTransform == nil || a.PromptTransform.Op != "append" || a.PromptTransform.Text != "\nUse the variant." {
		t.Fatalf("PromptTransform = %+v", a.PromptTransform)
		panic("unreachable")
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
				panic("unreachable")
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
				panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
		panic("unreachable")
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
				panic("unreachable")
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
				panic("unreachable")
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
		Roles:   []string{"implementation"},
		Variants: []Variant{
			{ID: "explicit-medium", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
			{ID: "omitted-effort", Provider: "claude", Model: "sonnet", ReasoningEffort: "", Weight: 1},
		},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
		panic("unreachable")
	}
}

// TestConfigValidatePromptSkillRejectsRolelessMixedEffort covers the ambiguous
// case: an experiment declaring no role matches every role (see roleMatches),
// so an omitted reasoning_effort dispatches at whatever baseline the step it
// lands on carries. Mixing it with an explicit level would silently confound
// the prompt comparison with an effort change, so it must be rejected until the
// operator declares the role.
func TestConfigValidatePromptSkillRejectsRolelessMixedEffort(t *testing.T) {
	for _, effort := range []string{"medium", "high"} {
		t.Run(effort, func(t *testing.T) {
			cfg := Config{Experiments: []Experiment{{
				ID:      "prompt-roleless-effort",
				Kind:    "prompt",
				Subject: &Subject{StepID: "implement"},
				Variants: []Variant{
					{ID: "omitted-effort", Provider: "claude", Model: "sonnet", Weight: 1},
					{ID: "explicit-effort", Provider: "claude", Model: "sonnet", ReasoningEffort: effort, Weight: 1},
				},
			}}}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate should reject a roleless mix of omitted and explicit reasoning_effort")
				panic("unreachable")
			}
			if !strings.Contains(err.Error(), "reasoning_effort") {
				t.Fatalf("error %q does not mention reasoning_effort", err)
			}
		})
	}
}

// TestConfigValidatePromptSkillAllowsRolelessOmittedEffort proves the
// ambiguity only bites a mix: leaving the level off every arm stays valid, so
// the common roleless prompt experiment is unaffected.
func TestConfigValidatePromptSkillAllowsRolelessOmittedEffort(t *testing.T) {
	cfg := Config{Experiments: []Experiment{{
		ID:      "prompt-roleless-omitted",
		Kind:    "prompt",
		Subject: &Subject{StepID: "implement"},
		Variants: []Variant{
			{ID: "a", Provider: "claude", Model: "sonnet", Weight: 1},
			{ID: "b", Provider: "claude", Model: "sonnet", Weight: 1},
		},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
		panic("unreachable")
	}
}

// TestConfigValidatePromptSkillResolvesEmptyEffortAgainstRoleBaseline covers
// roles whose built-in baseline is not the global default: on a review
// experiment an omitted reasoning_effort dispatches at "high", so pinning
// "high" explicitly on one arm is homogeneous with omitting it on another,
// while pinning the global "medium" is a genuine mismatch.
func TestConfigValidatePromptSkillResolvesEmptyEffortAgainstRoleBaseline(t *testing.T) {
	tests := []struct {
		name       string
		roles      []string
		effort     string
		wantReject bool
	}{
		{name: "review omitted vs explicit baseline", roles: []string{"review"}, effort: "high"},
		{name: "review omitted vs global default", roles: []string{"review"}, effort: "medium", wantReject: true},
		{name: "implementation omitted vs global default", roles: []string{"implementation"}, effort: "medium"},
		{name: "implementation omitted vs explicit high", roles: []string{"implementation"}, effort: "high", wantReject: true},
		{
			// plan resolves "high", test-runner resolves "medium": an
			// omitted effort is ambiguous, so it only matches another
			// omitted one.
			name:       "roles with disagreeing baselines stay ambiguous",
			roles:      []string{"plan", "test-runner"},
			effort:     "high",
			wantReject: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Experiments: []Experiment{{
				ID:      "prompt-role-effort",
				Kind:    "prompt",
				Subject: &Subject{StepID: "implement"},
				Roles:   tt.roles,
				Variants: []Variant{
					{ID: "omitted-effort", Provider: "claude", Model: "sonnet", Weight: 1},
					{ID: "explicit-effort", Provider: "claude", Model: "sonnet", ReasoningEffort: tt.effort, Weight: 1},
				},
			}}}
			err := cfg.Validate()
			if tt.wantReject {
				if err == nil {
					t.Fatal("Validate should reject a reasoning_effort mismatch")
					panic("unreachable")
				}
				if !strings.Contains(err.Error(), "reasoning_effort") {
					t.Fatalf("error %q does not mention reasoning_effort", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
				panic("unreachable")
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
		panic("unreachable")
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
				panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
				panic("unreachable")
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
		panic("unreachable")
	}

	cfg.Experiments[0].Variants[1].Weight = 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate should reject duplicate positive-weight variant ids")
		panic("unreachable")
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
			panic("unreachable")
		}
		if a.VariantID != "passing" {
			t.Fatalf("VariantID = %q, want passing (failing digest must be excluded)", a.VariantID)
		}
	}
}

func TestSelectEligibleNilEvalPassedUnchanged(t *testing.T) {
	cfg := enabledDefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate: %v", err)
		panic("unreachable")
	}
	want, ok, err := SelectEligible(cfg, "task-1", "implementation", "implement", nil)
	if err != nil || !ok {
		t.Fatalf("SelectEligible: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	got, ok, err := SelectEligibleWithEval(cfg, "task-1", "implementation", "implement", nil, nil)
	if err != nil || !ok {
		t.Fatalf("SelectEligibleWithEval(evalPassed=nil): ok=%v err=%v", ok, err)
		panic("unreachable")
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

func canaryConfig(canary CanaryPolicy) Config {
	enabled := true
	return Config{
		Enabled: &enabled,
		Experiments: []Experiment{{
			ID:             "canary-exp",
			Enabled:        &enabled,
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Canary:         &canary,
			Variants: []Variant{
				{ID: "baseline", Provider: "claude", Model: "sonnet", Weight: 1},
				{ID: "candidate", Provider: "codex", Model: "gpt", Weight: 1},
			},
		}},
	}
}

func TestSelectCanary_InsufficientCohortForcesBaseline(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 100, MinCohort: 20})
	cohortObserved := func(string) (int, bool) { return 5, true } // fresh but below MinCohort

	for _, taskID := range []string{"task-1", "task-2", "task-3"} {
		a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: taskID, Role: "implementation"}, nil, nil, cohortObserved)
		if err != nil || !ok {
			t.Fatalf("Select: ok=%v err=%v", ok, err)
			panic("unreachable")
		}
		if a.VariantID != "baseline" || a.Provider != "claude" {
			t.Fatalf("VariantID/Provider = %q/%q, want baseline/claude (insufficient cohort must force baseline)", a.VariantID, a.Provider)
		}
		if a.RoutingReason != "canary_baseline" {
			t.Fatalf("RoutingReason = %q, want canary_baseline", a.RoutingReason)
		}
	}
}

func TestSelectCanary_StaleCohortForcesBaseline(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 100, MinCohort: 0})
	cohortObserved := func(string) (int, bool) { return 1000, false } // plenty of data, but stale

	a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: "task-1", Role: "implementation"}, nil, nil, cohortObserved)
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if a.VariantID != "baseline" {
		t.Fatalf("VariantID = %q, want baseline (stale cohort must force baseline even with data)", a.VariantID)
	}
}

func TestSelectCanary_NilCohortObservedFailsClosedToBaseline(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 100, MinCohort: 0})

	a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: "task-1", Role: "implementation"}, nil, nil, nil)
	if err != nil || !ok {
		t.Fatalf("Select: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if a.VariantID != "baseline" {
		t.Fatalf("VariantID = %q, want baseline (nil cohortObserved must fail closed)", a.VariantID)
	}

	// SelectEligibleForContext (no cohort awareness at all) must behave
	// identically — a canary experiment is never silently treated as a
	// plain, uncapped A/B experiment by the existing entry point.
	a2, ok2, err2 := SelectEligibleForContext(cfg, SelectionContext{TaskID: "task-1", Role: "implementation"}, nil, nil)
	if err2 != nil || !ok2 || a2.VariantID != "baseline" {
		t.Fatalf("SelectEligibleForContext: a=%+v ok=%v err=%v, want baseline/true/nil", a2, ok2, err2)
		panic("unreachable")
	}
}

func TestSelectCanary_PercentBoundIsBoundedAndReproducible(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 20, MinCohort: 0})
	cohortObserved := func(string) (int, bool) { return 100, true }

	nonBaseline := 0
	const n = 2000
	firstPass := make(map[string]string, n)
	for i := range n {
		taskID := fmt.Sprintf("task-%d", i)
		a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: taskID, Role: "implementation"}, nil, nil, cohortObserved)
		if err != nil || !ok {
			t.Fatalf("Select: ok=%v err=%v", ok, err)
			panic("unreachable")
		}
		firstPass[taskID] = a.VariantID
		if a.VariantID == "candidate" {
			nonBaseline++
		}
	}
	// PercentBound=20 caps the eligible-for-non-baseline slice at 20% of
	// keys; within that slice the normal 50/50 weighted draw between
	// baseline/candidate applies, so candidate's share should land well
	// under 20% and well above 0%.
	if nonBaseline == 0 || nonBaseline > n/4 {
		t.Fatalf("candidate assignments = %d/%d, want roughly bounded near PercentBound/2 (%%), got outside (0, %d]", nonBaseline, n, n/4)
	}

	// Reproducible: same keys, repeated, must select identically.
	for i := range n {
		taskID := fmt.Sprintf("task-%d", i)
		a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: taskID, Role: "implementation"}, nil, nil, cohortObserved)
		if err != nil || !ok {
			t.Fatalf("Select: ok=%v err=%v", ok, err)
			panic("unreachable")
		}
		if a.VariantID != firstPass[taskID] {
			t.Fatalf("task %s selected %q then %q, want reproducible/stable assignment", taskID, firstPass[taskID], a.VariantID)
		}
	}
}

func TestSelectCanary_ZeroPercentBoundAlwaysBaseline(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 0, MinCohort: 0})
	cohortObserved := func(string) (int, bool) { return 1000, true }

	for i := range 200 {
		taskID := fmt.Sprintf("task-%d", i)
		a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: taskID, Role: "implementation"}, nil, nil, cohortObserved)
		if err != nil || !ok {
			t.Fatalf("Select: ok=%v err=%v", ok, err)
			panic("unreachable")
		}
		if a.VariantID != "baseline" {
			t.Fatalf("task %s selected %q, want baseline (percent_bound=0 must forbid all canary traffic)", taskID, a.VariantID)
		}
	}
}

func TestSelectCanary_BaselineProviderIneligibleDefersToPlainSelection(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 100, MinCohort: 0})
	providerAllowed := func(p string) bool { return p != "claude" } // baseline's provider is unavailable

	a, ok, err := SelectEligibleForContextWithCohort(cfg, SelectionContext{TaskID: "task-1", Role: "implementation"}, providerAllowed, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}
	if ok {
		t.Fatalf("Select ok=true a=%+v, want ok=false (defer to plain provider selection/failover)", a)
	}
}

func TestValidateExperiment_CanaryRequiresKnownBaselineVariant(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "does-not-exist", PercentBound: 50, MinCohort: 0})
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for unknown canary baseline_variant_id")
		panic("unreachable")
	}
}

func TestValidateExperiment_CanaryRejectsZeroWeightBaseline(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 50, MinCohort: 0})
	cfg.Experiments[0].Variants[0].Weight = 0 // baseline retired via zero weight
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error: a zero-weight (disabled) variant must not be a canary baseline")
		panic("unreachable")
	}
}

func TestValidateExperiment_CanaryPercentBoundOutOfRange(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 101, MinCohort: 0})
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for percent_bound > 100")
		panic("unreachable")
	}
}

func TestValidateExperiment_CanaryNegativeMinCohort(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 50, MinCohort: -1})
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for negative min_cohort")
		panic("unreachable")
	}
}

func TestCloneConfig_DeepCopiesCanary(t *testing.T) {
	cfg := canaryConfig(CanaryPolicy{BaselineVariantID: "baseline", PercentBound: 50, MinCohort: 10})
	cp := CloneConfig(cfg)
	cp.Experiments[0].Canary.PercentBound = 99
	if cfg.Experiments[0].Canary.PercentBound != 50 {
		t.Fatalf("original Canary mutated via clone: %d, want unchanged 50", cfg.Experiments[0].Canary.PercentBound)
	}
}

// TestWithoutInvalidExperimentsKeepsDispatchAlive covers the migration path for
// a config a code change retroactively invalidated: selection validates each
// experiment as it matches and propagates the error to the dispatcher, so an
// invalid experiment must be dropped (and reported) at load rather than left to
// wedge every role it targets.
func TestWithoutInvalidExperimentsKeepsDispatchAlive(t *testing.T) {
	good := Experiment{
		ID:       "good",
		Roles:    []string{"review"},
		Variants: []Variant{{ID: "a", Provider: "claude", Model: "opus", Weight: 1}},
	}
	// Roleless prompt experiment mixing an omitted and an explicit effort:
	// valid before the per-role baselines were retuned, ambiguous after.
	bad := Experiment{
		ID:      "bad",
		Kind:    "prompt",
		Subject: &Subject{StepID: "implement"},
		Variants: []Variant{
			{ID: "a", Provider: "claude", Model: "sonnet", Weight: 1},
			{ID: "b", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", Weight: 1},
		},
	}
	cfg := Config{Experiments: []Experiment{good, bad}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("precondition: the bad experiment should fail validation")
		panic("unreachable")
	}

	var dropped []string
	got := cfg.WithoutInvalidExperiments(func(id string, err error) {
		if err == nil {
			t.Errorf("report called with nil error for %q", id)
		}
		dropped = append(dropped, id)
	})

	if len(dropped) != 1 || dropped[0] != "bad" {
		t.Fatalf("dropped = %v, want [bad]", dropped)
	}
	if len(got.Experiments) != 1 || got.Experiments[0].ID != "good" {
		t.Fatalf("kept experiments = %+v, want just the good one", got.Experiments)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("survivors must validate: %v", err)
		panic("unreachable")
	}
	if len(cfg.Experiments) != 2 {
		t.Fatalf("receiver mutated: len = %d, want 2", len(cfg.Experiments))
	}

	// A selection that previously errored out now routes normally.
	enabled := true
	got.Enabled = &enabled
	if _, _, err := Select(got, "task-1", "review", "review"); err != nil {
		t.Fatalf("Select after drop: %v", err)
		panic("unreachable")
	}
}
