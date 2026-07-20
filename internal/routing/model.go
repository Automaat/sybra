// Package routing turns the periodic evaluation scorecard into bounded,
// versioned weight nudges for internal/abtest's per-experiment variants —
// "provider A lands more and costs less than provider B on this role, so
// shift a little more traffic its way" — without ever gating dispatch on
// cost or stopping work for a human. Every function in model.go/score.go/
// plan.go is pure and deterministic; the only I/O in this package is
// Store's atomic overlay read/write and Service's ticker.
package routing

import "time"

// ScoreInputs are the raw evaluation signals a Score was derived from,
// retained so the overlay and audit event can explain a weight change
// without re-deriving it from the evaluation report.
type ScoreInputs struct {
	LandedWilsonLower      float64 `json:"landedWilsonLower" yaml:"landed_wilson_lower"`
	MergeWilsonLower       float64 `json:"mergeWilsonLower" yaml:"merge_wilson_lower"`
	CIFirstPassWilsonLower float64 `json:"ciFirstPassWilsonLower" yaml:"ci_first_pass_wilson_lower"`
	ReworkWilsonUpper      float64 `json:"reworkWilsonUpper" yaml:"rework_wilson_upper"`
	FailureWilsonUpper     float64 `json:"failureWilsonUpper" yaml:"failure_wilson_upper"`
	CostPerLanded          float64 `json:"costPerLanded" yaml:"cost_per_landed"`
	DurationP50S           float64 `json:"durationP50S" yaml:"duration_p50s"`
}

// Score is the composite outcome score for one (experiment, variant),
// derived by ScoreVariants from an evaluation.ComparisonBreakdown row.
type Score struct {
	ExperimentID       string      `json:"experimentId"`
	VariantID          string      `json:"variantId"`
	Value              float64     `json:"value"`
	Runs               int         `json:"runs"`
	ResolvedRuns       int         `json:"resolvedRuns"`
	InsufficientData   bool        `json:"insufficientData"`
	SkillParityUnknown bool        `json:"skillParityUnknown"`
	Inputs             ScoreInputs `json:"inputs"`
}

// Coefficients weights each score input's contribution. Landed/merge/
// CI-first-pass are added (higher is better); rework/failure/cost/duration
// are subtracted (higher is worse). Cost and duration are normalized to a
// 0..1-clamped range before their weight is applied, via CostNormalizer/
// DurationNormalizer, so an outlier run cannot swamp the composite score.
type Coefficients struct {
	LandedWeight       float64
	MergeWeight        float64
	CIFirstPassWeight  float64
	ReworkWeight       float64
	FailureWeight      float64
	CostWeight         float64
	DurationWeight     float64
	CostNormalizer     float64
	DurationNormalizer float64
}

// PlanOptions controls PlanWeights' floor/budget/step behavior.
type PlanOptions struct {
	// WeightBudget is the total weight PlanWeights distributes across one
	// experiment's variants each tick, floor allocation included.
	WeightBudget int
	// FloorWeight is the minimum weight every configured variant keeps
	// regardless of score — the exploration guarantee.
	FloorWeight int
	// MaxStep bounds how far a single call may move one variant's weight
	// away from its current value, up or down.
	MaxStep int
	// MinSamplesToShift is the ResolvedRuns threshold below which a variant
	// is treated as insufficiently sampled and held at FloorWeight instead
	// of being ranked.
	MinSamplesToShift int
}

// WeightPlan is the output of PlanWeights: the new weight per (experiment,
// variant), plus whether anything actually changed from current.
type WeightPlan struct {
	Changed     bool
	Experiments map[string]map[string]int
}

// Overlay is the persisted routing state: one weight-plan generation plus
// the score inputs that produced it, for audit/explainability and for
// PlanWeights' next-tick step clamping.
type Overlay struct {
	Version     int                 `json:"version" yaml:"version"`
	GeneratedAt time.Time           `json:"generatedAt" yaml:"generated_at"`
	Experiments []OverlayExperiment `json:"experiments" yaml:"experiments"`
}

// OverlayExperiment holds one experiment's variant weights and scores.
type OverlayExperiment struct {
	ExperimentID string           `json:"experimentId" yaml:"experiment_id"`
	Variants     []OverlayVariant `json:"variants" yaml:"variants"`
}

// OverlayVariant is one variant's applied weight and the score that set it.
type OverlayVariant struct {
	VariantID          string      `json:"variantId" yaml:"variant_id"`
	Weight             int         `json:"weight" yaml:"weight"`
	Score              float64     `json:"score" yaml:"score"`
	Inputs             ScoreInputs `json:"inputs" yaml:"inputs"`
	Runs               int         `json:"runs" yaml:"runs"`
	ResolvedRuns       int         `json:"resolvedRuns" yaml:"resolved_runs"`
	InsufficientData   bool        `json:"insufficientData" yaml:"insufficient_data"`
	SkillParityUnknown bool        `json:"skillParityUnknown" yaml:"skill_parity_unknown"`
}

// WeightAt returns the previously-applied weight for (experimentID,
// variantID), and whether one was recorded — 0/false for a variant the
// overlay has never scored (e.g. brand new, or never run).
func (o Overlay) WeightAt(experimentID, variantID string) (int, bool) {
	for _, exp := range o.Experiments {
		if exp.ExperimentID != experimentID {
			continue
		}
		for _, v := range exp.Variants {
			if v.VariantID == variantID {
				return v.Weight, true
			}
		}
	}
	return 0, false
}
