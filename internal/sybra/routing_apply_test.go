package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/config"
)

// TestApplyRoutingWeights_PropagatesToConfigAndOrchestrator verifies the
// routing.WeightApplier push path reaches the two directly-inspectable live
// selection sites: a.cfg.ABTesting (what app_human_review.go and
// svc_tasks.go's staff-review dispatch read on every call) and
// orchSvc.abTesting (what StartOrchestrator dispatches the brain under).
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

	if a.cfg.ABTesting.WeightsVersion == nil || *a.cfg.ABTesting.WeightsVersion != 7 {
		t.Fatalf("cfg.ABTesting.WeightsVersion = %+v, want 7", a.cfg.ABTesting.WeightsVersion)
	}
	if len(a.cfg.ABTesting.Experiments) != 1 || a.cfg.ABTesting.Experiments[0].Variants[0].Weight != 9 {
		t.Fatalf("cfg.ABTesting.Experiments = %+v, want weight 9", a.cfg.ABTesting.Experiments)
	}
	if orchSvc.abTesting.WeightsVersion == nil || *orchSvc.abTesting.WeightsVersion != 7 {
		t.Fatalf("orchSvc.abTesting.WeightsVersion = %+v, want 7", orchSvc.abTesting.WeightsVersion)
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
