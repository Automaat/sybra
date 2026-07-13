package agent

import (
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
)

func stubProviderCLIAvailable(t *testing.T, avail func(string) bool) {
	t.Helper()
	prev := providerCLIAvailable
	providerCLIAvailable = avail
	t.Cleanup(func() { providerCLIAvailable = prev })
}

func reviewExperiment(variants ...abtest.Variant) abtest.Config {
	enabled := true
	return abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:       "review-test",
			Enabled:  &enabled,
			Roles:    []string{"review"},
			Variants: variants,
		}},
	}
}

func TestApplyABVariant_DisabledLeavesConfigUnchanged(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, abtest.Config{}, "task-1", "review")

	if cfg.Provider != "" || cfg.Model != "opus" || cfg.ExperimentID != "" {
		t.Fatalf("unchanged config expected, got provider=%q model=%q exp=%q", cfg.Provider, cfg.Model, cfg.ExperimentID)
	}
}

func TestApplyABVariant_AssignsEligibleProvider(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{ID: "codex-only", Provider: "codex", Model: "gpt-5.5", Weight: 1})

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "review")

	if cfg.Provider != "codex" || cfg.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want codex/gpt-5.5", cfg.Provider, cfg.Model)
	}
	if cfg.ExperimentID != "review-test" || cfg.VariantID != "codex-only" {
		t.Fatalf("attribution = %q/%q, want review-test/codex-only", cfg.ExperimentID, cfg.VariantID)
	}
	if cfg.DisableProviderFailover {
		t.Fatal("DisableProviderFailover = true, want false (availability preferred)")
	}
}

func TestApplyABVariant_SkipsProviderWithMissingCLI(t *testing.T) {
	stubProviderCLIAvailable(t, func(p string) bool { return p != "copilot" })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{ID: "copilot-only", Provider: "copilot", Model: "gemini-3.1-pro", Weight: 1})

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "review")

	if cfg.Provider != "" {
		t.Fatalf("Provider = %q, want empty — a variant whose CLI is absent must not be assigned", cfg.Provider)
	}
	if cfg.Model != "opus" {
		t.Fatalf("Model = %q, want opus (unchanged base)", cfg.Model)
	}
}

func TestApplyABVariant_EvalGateExcludesDigestedVariant(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	m.SetEvalPassed(func(variantID, digest string) bool { return false })
	ab := reviewExperiment(abtest.Variant{ID: "codex-digested", Provider: "codex", Model: "gpt-5.5", Digest: "d1", Weight: 1})

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "review")

	if cfg.Provider != "" {
		t.Fatalf("Provider = %q, want empty — an eval-failed digested variant must not enroll", cfg.Provider)
	}
}

func TestApplyABVariant_AppliesPromptTransform(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{
		ID: "tightened", Provider: "claude", Model: "sonnet", Weight: 1,
		PromptTransform: &abtest.PromptTransform{Op: "append", Text: "\nextra instructions"},
	})

	cfg := m.ApplyABVariant(RunConfig{Prompt: "base prompt"}, ab, "task-1", "review")

	if cfg.Prompt != "base prompt\nextra instructions" {
		t.Fatalf("Prompt = %q, want base prompt + appended transform text", cfg.Prompt)
	}
}

func TestApplyABVariant_NilPromptTransformLeavesPromptUnchanged(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{ID: "codex-only", Provider: "codex", Model: "gpt-5.5", Weight: 1})

	cfg := m.ApplyABVariant(RunConfig{Prompt: "base prompt"}, ab, "task-1", "review")

	if cfg.Prompt != "base prompt" {
		t.Fatalf("Prompt = %q, want unchanged base prompt", cfg.Prompt)
	}
}

func TestApplyABVariant_RoleMismatchLeavesConfigUnchanged(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{ID: "codex-only", Provider: "codex", Model: "gpt-5.5", Weight: 1})

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "implementation")

	if cfg.Provider != "" || cfg.ExperimentID != "" {
		t.Fatalf("unmatched role should leave config unchanged, got provider=%q exp=%q", cfg.Provider, cfg.ExperimentID)
	}
}
