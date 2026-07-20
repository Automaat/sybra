package routing

import "testing"

func TestPlanWeights_NoOpWhenNothingChanges(t *testing.T) {
	current := map[string]map[string]int{"exp": {"v1": 5, "v2": 5}}
	scores := []Score{
		{ExperimentID: "exp", VariantID: "v1", Value: 1, ResolvedRuns: 30},
		{ExperimentID: "exp", VariantID: "v2", Value: 1, ResolvedRuns: 30},
	}
	// Budget/floor/step chosen so the ranked target exactly matches current:
	// equal scores tie-break deterministically but a budget that only covers
	// the floor produces no ranked delta either way.
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 10, FloorWeight: 5, MaxStep: 5, MinSamplesToShift: 20})
	if plan.Changed {
		t.Fatalf("PlanWeights() = %+v, want unchanged", plan)
	}
	if len(plan.Experiments) != 0 {
		t.Fatalf("PlanWeights().Experiments = %+v, want empty", plan.Experiments)
	}
}

func TestPlanWeights_EmptyInputIsNoOp(t *testing.T) {
	plan := PlanWeights(nil, nil, PlanOptions{})
	if plan.Changed || len(plan.Experiments) != 0 {
		t.Fatalf("PlanWeights(nil, nil) = %+v, want no-op", plan)
	}
}

func TestPlanWeights_IgnoresScoreOnlyRetiredCohorts(t *testing.T) {
	current := map[string]map[string]int{"exp": {"live": 1}}
	scores := []Score{
		{ExperimentID: "exp-gone", VariantID: "a", Value: 100, ResolvedRuns: 100},
		{ExperimentID: "exp-gone", VariantID: "b", Value: 1, ResolvedRuns: 100},
		{ExperimentID: "exp", VariantID: "variant-gone", Value: 100, ResolvedRuns: 100},
	}

	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 20, FloorWeight: 1, MaxStep: 100, MinSamplesToShift: 20})

	if plan.Changed || len(plan.Experiments) != 0 {
		t.Fatalf("PlanWeights() = %+v, want no-op for score-only retired cohorts", plan)
	}
}

func TestPlanWeights_FloorKeepsLowSampleExplorationTraffic(t *testing.T) {
	current := map[string]map[string]int{"exp": {"winner": 1, "new": 1, "insufficient": 1}}
	scores := []Score{
		{ExperimentID: "exp", VariantID: "winner", Value: 10, ResolvedRuns: 100},
		// "new" never appears in scores at all (zero runs) — must still float
		// at the floor via the current-weights universe.
		{ExperimentID: "exp", VariantID: "insufficient", Value: 10, ResolvedRuns: 3, InsufficientData: true},
	}
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 100, FloorWeight: 2, MaxStep: 50, MinSamplesToShift: 20})
	if !plan.Changed {
		t.Fatalf("PlanWeights() = %+v, want changed", plan)
	}
	got := plan.Experiments["exp"]
	if got["new"] < 2 {
		t.Fatalf("new.weight = %d, want >= floor (2)", got["new"])
	}
	if got["insufficient"] < 2 {
		t.Fatalf("insufficient.weight = %d, want >= floor (2)", got["insufficient"])
	}
	if got["winner"] <= got["new"] {
		t.Fatalf("winner.weight = %d, want > new.weight = %d", got["winner"], got["new"])
	}
}

func TestPlanWeights_CostLowersButNeverZeroesWeight(t *testing.T) {
	current := map[string]map[string]int{"exp": {"cheap": 1, "expensive": 1}}
	scores := []Score{
		{ExperimentID: "exp", VariantID: "cheap", Value: 10, ResolvedRuns: 50},
		// expensive scores far below zero (heavy cost penalty already applied
		// upstream by ScoreVariants) — PlanWeights must still floor it, never 0.
		{ExperimentID: "exp", VariantID: "expensive", Value: -100, ResolvedRuns: 50},
	}
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 50, FloorWeight: 1, MaxStep: 50, MinSamplesToShift: 20})
	got := plan.Experiments["exp"]
	if got["expensive"] < 1 {
		t.Fatalf("expensive.weight = %d, want >= 1 (floor), never 0", got["expensive"])
	}
	if got["cheap"] <= got["expensive"] {
		t.Fatalf("cheap.weight = %d, want > expensive.weight = %d", got["cheap"], got["expensive"])
	}
}

func TestPlanWeights_StepIsBounded(t *testing.T) {
	current := map[string]map[string]int{"exp": {"a": 1, "b": 1}}
	scores := []Score{
		// Huge score gap that would otherwise pull "a" far above "b" in one tick.
		{ExperimentID: "exp", VariantID: "a", Value: 1000, ResolvedRuns: 100},
		{ExperimentID: "exp", VariantID: "b", Value: 0, ResolvedRuns: 100},
	}
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 1000, FloorWeight: 1, MaxStep: 3, MinSamplesToShift: 20})
	got := plan.Experiments["exp"]
	if delta := got["a"] - 1; delta > 3 {
		t.Fatalf("a.weight moved by %d in one tick, want <= 3 (MaxStep)", delta)
	}
}

func TestPlanWeights_StepBoundedDownward(t *testing.T) {
	// A variant currently far above the floor (e.g. from a prior generation)
	// whose score collapses must still only step down by MaxStep, not
	// immediately snap to the floor.
	current := map[string]map[string]int{"exp": {"a": 50, "b": 1}}
	scores := []Score{
		{ExperimentID: "exp", VariantID: "a", Value: -100, ResolvedRuns: 100},
		{ExperimentID: "exp", VariantID: "b", Value: 100, ResolvedRuns: 100},
	}
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 100, FloorWeight: 1, MaxStep: 4, MinSamplesToShift: 20})
	got := plan.Experiments["exp"]
	if delta := 50 - got["a"]; delta > 4 {
		t.Fatalf("a.weight dropped by %d in one tick, want <= 4 (MaxStep)", delta)
	}
}

func TestPlanWeights_BelowMinSamplesTreatedAsExplorationOnly(t *testing.T) {
	current := map[string]map[string]int{"exp": {"proven": 1, "fresh": 1}}
	scores := []Score{
		{ExperimentID: "exp", VariantID: "proven", Value: 1, ResolvedRuns: 100},
		// High score but too few resolved runs to be trusted for ranking.
		{ExperimentID: "exp", VariantID: "fresh", Value: 1000, ResolvedRuns: 2},
	}
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 100, FloorWeight: 1, MaxStep: 100, MinSamplesToShift: 20})
	got := plan.Experiments["exp"]
	if got["fresh"] != 1 {
		t.Fatalf("fresh.weight = %d, want held at floor (1) despite high score", got["fresh"])
	}
	if got["proven"] <= got["fresh"] {
		t.Fatalf("proven.weight = %d, want > fresh.weight = %d", got["proven"], got["fresh"])
	}
}

func TestPlanWeights_MultipleExperimentsIndependent(t *testing.T) {
	current := map[string]map[string]int{
		"exp-a": {"v1": 1, "v2": 1},
		"exp-b": {"v1": 1, "v2": 1},
	}
	scores := []Score{
		{ExperimentID: "exp-a", VariantID: "v1", Value: 10, ResolvedRuns: 100},
		{ExperimentID: "exp-a", VariantID: "v2", Value: 1, ResolvedRuns: 100},
		// exp-b's variants are both below MinSamplesToShift, so neither is
		// rank-eligible — both stay at the floor, matching current: no change.
		{ExperimentID: "exp-b", VariantID: "v1", Value: 1, ResolvedRuns: 5},
		{ExperimentID: "exp-b", VariantID: "v2", Value: 1, ResolvedRuns: 5},
	}
	plan := PlanWeights(scores, current, PlanOptions{WeightBudget: 10, FloorWeight: 1, MaxStep: 5, MinSamplesToShift: 20})
	if _, ok := plan.Experiments["exp-a"]; !ok {
		t.Fatalf("plan.Experiments missing exp-a: %+v", plan.Experiments)
	}
	if _, ok := plan.Experiments["exp-b"]; ok {
		t.Fatalf("plan.Experiments should not include unchanged exp-b: %+v", plan.Experiments)
	}
}
