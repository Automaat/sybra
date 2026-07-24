package routing

import (
	"testing"

	"github.com/Automaat/sybra/internal/evaluation"
)

func TestScoreVariants_SkipsRowsWithoutExperimentAttribution(t *testing.T) {
	rows := []evaluation.ComparisonBreakdown{
		{ExperimentID: "", VariantID: "v1"},
		{ExperimentID: "exp", VariantID: ""},
	}
	got := ScoreVariants(rows, DefaultTestCoefficients())
	if len(got) != 0 {
		t.Fatalf("ScoreVariants() = %+v, want empty", got)
	}
}

func TestScoreVariants_Deterministic(t *testing.T) {
	rows := []evaluation.ComparisonBreakdown{
		{
			ExperimentID: "exp-a", VariantID: "v2",
			LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.5},
			CostPerLanded:  2,
		},
		{
			ExperimentID: "exp-a", VariantID: "v1",
			LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.9},
			CostPerLanded:  1,
		},
	}
	coef := DefaultTestCoefficients()
	a := ScoreVariants(rows, coef)
	b := ScoreVariants(rows, coef)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("ScoreVariants() len = %d/%d, want 2/2", len(a), len(b))
	}
	// Sorted by (ExperimentID, VariantID): v1 before v2.
	if a[0].VariantID != "v1" || a[1].VariantID != "v2" {
		t.Fatalf("ScoreVariants() order = %+v, want v1 before v2", a)
	}
	if a[0].Value != b[0].Value || a[1].Value != b[1].Value {
		t.Fatalf("ScoreVariants() not deterministic: %+v vs %+v", a, b)
	}
	// v1 has both a higher landed rate and a lower cost, so it must outscore v2.
	if a[0].Value <= a[1].Value {
		t.Fatalf("v1.Value = %v, want > v2.Value = %v", a[0].Value, a[1].Value)
	}
}

func TestScoreVariants_HigherCostLowersScoreButRowsStillEmitted(t *testing.T) {
	base := evaluation.ComparisonBreakdown{
		ExperimentID:   "exp",
		VariantID:      "v",
		LandedEstimate: evaluation.RateEstimate{WilsonLower: 0.8},
	}
	cheap := base
	cheap.CostPerLanded = 0.5
	expensive := base
	expensive.CostPerLanded = 50 // far beyond CostNormalizer, clamps to full penalty

	coef := DefaultTestCoefficients()
	cheapScore := ScoreVariants([]evaluation.ComparisonBreakdown{cheap}, coef)[0]
	expensiveScore := ScoreVariants([]evaluation.ComparisonBreakdown{expensive}, coef)[0]

	if expensiveScore.Value >= cheapScore.Value {
		t.Fatalf("expensive.Value = %v, want < cheap.Value = %v", expensiveScore.Value, cheapScore.Value)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		name  string
		v     float64
		scale float64
		want  float64
	}{
		{"zero scale disables", 5, 0, 0},
		{"negative value clamps to zero", -5, 10, 0},
		{"within range", 5, 10, 0.5},
		{"beyond range clamps to one", 50, 10, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalize(tc.v, tc.scale); got != tc.want {
				t.Fatalf("normalize(%v, %v) = %v, want %v", tc.v, tc.scale, got, tc.want)
			}
		})
	}
}

// DefaultTestCoefficients mirrors config.DefaultRoutingCoefficients without
// importing internal/config (which would cycle back through internal/routing
// via nothing today, but keeps this package's tests independent of config's
// default-tuning choices).
func DefaultTestCoefficients() Coefficients {
	return Coefficients{
		LandedWeight:       1.0,
		MergeWeight:        0.5,
		CIFirstPassWeight:  0.25,
		ReworkWeight:       0.75,
		FailureWeight:      1.0,
		CostWeight:         0.2,
		DurationWeight:     0.1,
		CostNormalizer:     5.0,
		DurationNormalizer: 3600.0,
	}
}
