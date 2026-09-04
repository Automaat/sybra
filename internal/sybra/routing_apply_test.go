package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/config"
)

// TestApplyRoutingWeights_PropagatesToConfigAndOrchestrator verifies the
// routing.WeightApplier push path reaches the two directly-inspectable live
// selection sites: App's live A/B snapshot (what human-review and staff-review
// dispatch read on every call) and orchSvc.abTesting (what StartOrchestrator
// dispatches the brain under).
// workflowEngine.SetABTestingConfig and evaluationSvc.SetABTesting are each a
// trivial one-line delegation to an already-tested setter on those packages
// (see internal/workflow's SetABTestingConfig and
// internal/evaluation's TestService_SetABTesting_UpdatesNextScan); this test
// focuses on the two sites this package owns directly.
func TestApplyRoutingWeights_PropagatesToConfigAndOrchestrator(t *testing.T) {
	cfg := &config.Config{ABTesting: abtest.Config{MinSamplesPerVariant: 20}}
	orchSvc := &OrchestratorService{}
	a := &App{cfg: cfg, orchSvc: orchSvc}

	version := 7
	merged := abtest.Config{
		MinSamplesPerVariant: 20,
		WeightsVersion:       &version,
		Experiments: []abtest.Experiment{
			{ID: "exp", Variants: []abtest.Variant{{ID: "v1", Weight: 9}}},
		},
	}

	if err := a.applyRoutingWeights(merged); err != nil {
		t.Fatalf("applyRoutingWeights: %v", err)
	}

	got := a.abTestingConfig()
	if got.WeightsVersion == nil || *got.WeightsVersion != 7 {
		t.Fatalf("live ABTesting.WeightsVersion = %+v, want 7", got.WeightsVersion)
	}
	if len(got.Experiments) != 1 || got.Experiments[0].Variants[0].Weight != 9 {
		t.Fatalf("live ABTesting.Experiments = %+v, want weight 9", got.Experiments)
	}
	if orchSvc.abTesting.WeightsVersion == nil || *orchSvc.abTesting.WeightsVersion != 7 {
		t.Fatalf("orchSvc.abTesting.WeightsVersion = %+v, want 7", orchSvc.abTesting.WeightsVersion)
	}
}

func TestApplyRoutingWeights_DoesNotMutateBaseConfig(t *testing.T) {
	cfg := &config.Config{ABTesting: abtest.Config{MinSamplesPerVariant: 20}}
	a := &App{cfg: cfg}
	a.initializeABTesting(cfg.ABTesting)

	version := 7
	if err := a.applyRoutingWeights(abtest.Config{
		MinSamplesPerVariant: 20,
		WeightsVersion:       &version,
		Experiments: []abtest.Experiment{
			{ID: "exp", Variants: []abtest.Variant{{ID: "v1", Weight: 9}}},
		},
	}); err != nil {
		t.Fatalf("applyRoutingWeights: %v", err)
	}

	if cfg.ABTesting.WeightsVersion != nil || len(cfg.ABTesting.Experiments) != 0 {
		t.Fatalf("base cfg.ABTesting = %+v, want untouched operator base", cfg.ABTesting)
	}
	got := a.abTestingConfig()
	if got.WeightsVersion == nil || *got.WeightsVersion != 7 {
		t.Fatalf("live ABTesting.WeightsVersion = %+v, want 7", got.WeightsVersion)
	}
}

// TestApplyRoutingWeights_NilCollaboratorsSafe verifies every push is a
// no-op (not a panic) when a collaborator was never wired — the state of an
// App under test, or one built without a workflow engine/orchestrator.
func TestApplyRoutingWeights_NilCollaboratorsSafe(t *testing.T) {
	a := &App{}
	if err := a.applyRoutingWeights(abtest.Config{}); err != nil {
		t.Fatalf("applyRoutingWeights on a bare App: %v", err)
	}
}
