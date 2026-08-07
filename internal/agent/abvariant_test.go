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

func TestApplyABVariant_AppliesPromptTransform(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{
		ID:              "codex-prompt",
		Provider:        "codex",
		Model:           "gpt-5.5",
		PromptTransform: &abtest.PromptTransform{Op: "append", Text: "\nUse the review variant."},
		Weight:          1,
	})

	cfg := m.ApplyABVariant(RunConfig{Model: "opus", Prompt: "Run /staff-code-review"}, ab, "task-1", "review")

	if want := "Run /staff-code-review\nUse the review variant."; cfg.Prompt != want {
		t.Fatalf("Prompt = %q, want %q", cfg.Prompt, want)
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

func TestApplyABVariant_RoleMismatchLeavesConfigUnchanged(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := reviewExperiment(abtest.Variant{ID: "codex-only", Provider: "codex", Model: "gpt-5.5", Weight: 1})

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "implementation")

	if cfg.Provider != "" || cfg.ExperimentID != "" {
		t.Fatalf("unmatched role should leave config unchanged, got provider=%q exp=%q", cfg.Provider, cfg.ExperimentID)
	}
}

func canaryReviewConfig() abtest.Config {
	enabled := true
	return abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:      "review-canary",
			Enabled: &enabled,
			Roles:   []string{"review"},
			Canary:  &abtest.CanaryPolicy{BaselineVariantID: "claude-baseline", PercentBound: 100, MinCohort: 10},
			Variants: []abtest.Variant{
				{ID: "claude-baseline", Provider: "claude", Model: "opus", Weight: 1},
				{ID: "codex-candidate", Provider: "codex", Model: "gpt-5.5", Weight: 1},
			},
		}},
	}
}

// TestApplyABVariant_CanaryWithoutObserverFailsClosedToBaseline covers the
// default (SetCohortObserved never called) production posture: a canary
// experiment must never silently behave like an uncapped A/B experiment.
func TestApplyABVariant_CanaryWithoutObserverFailsClosedToBaseline(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	ab := canaryReviewConfig()

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "review")

	if cfg.Provider != "claude" || cfg.VariantID != "claude-baseline" {
		t.Fatalf("provider/variant = %q/%q, want claude/claude-baseline (no cohort observer -> fail closed to baseline)", cfg.Provider, cfg.VariantID)
	}
}

// TestApplyABVariant_CanaryCohortReadyEnrollsCandidate covers the "positive"
// canary path: once SetCohortObserved reports a fresh, sufficiently sampled
// cohort, the experiment's normal weighted draw is free to pick the
// non-baseline candidate.
func TestApplyABVariant_CanaryCohortReadyEnrollsCandidate(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m := &Manager{}
	m.SetCohortObserved(func(string) (int, bool) { return 100, true })
	ab := canaryReviewConfig()

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "review")

	if cfg.Provider != "claude" && cfg.Provider != "codex" {
		t.Fatalf("provider = %q, want claude or codex (cohort ready -> normal weighted draw)", cfg.Provider)
	}
	if cfg.ExperimentID != "review-canary" {
		t.Fatalf("ExperimentID = %q, want review-canary", cfg.ExperimentID)
	}
}

// TestApplyABVariant_CanaryFailoverIndependentOfAssignment covers "Keep
// provider health failover independent from experiment assignment" plus the
// acceptance criterion "failover during a canary": a RunConfig stamped by a
// canary/AB assignment must still fail over to a healthy peer through
// gateProvider exactly like any other assignment, with the experiment
// attribution left untouched by the failover decision.
func TestApplyABVariant_CanaryFailoverIndependentOfAssignment(t *testing.T) {
	stubProviderCLIAvailable(t, func(string) bool { return true })
	m, _ := newTestManager(t)
	m.SetCohortObserved(func(string) (int, bool) { return 100, true })
	gate := &fakeGate{
		// Healthy at assignment time: the canary baseline must be eligible
		// to enroll now. Failover is a genuinely separate, later decision —
		// see the gate mutation below, mirroring "an agent already running
		// when its own provider caps mid-run does not hot-swap" (failover is
		// decided fresh at each dispatch, not baked into assignment).
		healthy:  map[string]bool{"claude": true, "codex": true},
		failover: map[string]string{"claude": "codex"},
		reasons:  map[string]string{"claude": "logged_out"},
	}
	m.SetHealthGate(gate)
	enabled := true
	ab := abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:      "review-canary",
			Enabled: &enabled,
			Roles:   []string{"review"},
			Canary:  &abtest.CanaryPolicy{BaselineVariantID: "claude-baseline", PercentBound: 0, MinCohort: 0},
			Variants: []abtest.Variant{
				{ID: "claude-baseline", Provider: "claude", Model: "opus", Weight: 1},
				{ID: "codex-candidate", Provider: "codex", Model: "gpt-5.5", Weight: 1},
			},
		}},
	}

	cfg := m.ApplyABVariant(RunConfig{Model: "opus"}, ab, "task-1", "review")
	if cfg.Provider != "claude" || cfg.ExperimentID != "review-canary" || cfg.VariantID != "claude-baseline" {
		t.Fatalf("assignment = provider=%q exp=%q variant=%q, want claude/review-canary/claude-baseline (percent_bound=0 forces baseline)", cfg.Provider, cfg.ExperimentID, cfg.VariantID)
	}

	// Provider health changes only after assignment, at dispatch time —
	// exercising gateProvider's independent, later failover decision.
	gate.healthy["claude"] = false

	resolved, err := m.gateProvider(cfg)
	if err != nil {
		t.Fatalf("gateProvider: unexpected err: %v", err)
		panic("unreachable")
	}
	if resolved != "codex" {
		t.Fatalf("gateProvider resolved = %q, want codex (failover from unhealthy claude)", resolved)
	}
	// The experiment attribution set by ApplyABVariant must be untouched by
	// the independent failover decision.
	if cfg.ExperimentID != "review-canary" || cfg.VariantID != "claude-baseline" {
		t.Fatalf("experiment attribution mutated by failover: exp=%q variant=%q", cfg.ExperimentID, cfg.VariantID)
	}
}
