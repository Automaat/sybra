package config

// RoutingConfig controls the adaptive provider-routing service, which turns
// the periodic evaluation scorecard into bounded, versioned weight nudges for
// internal/abtest's per-experiment variants. It never writes to config.yaml —
// learned weights live in a side-store overlay (routing.Store) applied
// in-process — so a bad tick is trivially recoverable (delete the overlay,
// restart) and the operator's own ab_testing.yaml stays the source of truth
// for which variants exist at all.
//
// Enabled defaults to false: a fresh install computes and audits weight
// proposals (shadow mode) without ever pushing them live, matching
// PromptLabConfig's opt-in posture for a new autonomous loop.
//
// Coefficients is left zero-valued by most operators; applyRoutingDefaults
// fills any zero field from DefaultCoefficients so a partial override (e.g.
// only CostWeight) leaves every other term at its shipped default.
type RoutingConfig struct {
	Enabled       bool    `yaml:"enabled" json:"enabled"`
	IntervalHours float64 `yaml:"interval_hours" json:"intervalHours"`
	// WeightBudget is the total weight distributed across a single
	// experiment's variants each tick (floor allocation included). Higher
	// values let the top-ranked variant pull further ahead of the floor.
	WeightBudget int `yaml:"weight_budget" json:"weightBudget"`
	// FloorWeight is the minimum weight every configured variant keeps
	// regardless of score, guaranteeing exploration traffic to low-sample and
	// parity-unknown cohorts (acceptance criterion: "low-sample cohorts
	// retain exploration traffic").
	FloorWeight int `yaml:"floor_weight" json:"floorWeight"`
	// MaxStep bounds how far a single tick may move one variant's weight, up
	// or down, so a noisy score swing cannot re-route most traffic in one
	// generation.
	MaxStep int `yaml:"max_step" json:"maxStep"`
	// MinSamplesToShift is the resolved-run threshold below which a variant
	// is treated as insufficiently sampled and held at FloorWeight instead of
	// being ranked — distinct from ab_testing.min_samples_per_variant, which
	// gates the evaluation scorecard's InsufficientData display flag.
	MinSamplesToShift int                 `yaml:"min_samples_to_shift" json:"minSamplesToShift"`
	Coefficients      RoutingCoefficients `yaml:"coefficients" json:"coefficients"`
	// EvaluationMaxAgeHours bounds how old the evaluation report backing a
	// tick may be before it is treated as untrustworthy (see
	// evaluation.Trustworthy) and the tick rolls the overlay back to base
	// weights instead of promoting/expanding experiment traffic from it. <=0
	// disables the freshness check.
	EvaluationMaxAgeHours float64 `yaml:"evaluation_max_age_hours" json:"evaluationMaxAgeHours"`
}

// RoutingCoefficients weights each score input's contribution to a variant's
// composite outcome score. Positive fields reward the metric (landed, merge,
// CI-first-pass rate); the rework/failure/cost/duration fields are already
// applied as penalties by routing.ScoreVariants, so a larger value here means
// a stronger penalty, never a sign flip.
type RoutingCoefficients struct {
	LandedWeight      float64 `yaml:"landed_weight" json:"landedWeight"`
	MergeWeight       float64 `yaml:"merge_weight" json:"mergeWeight"`
	CIFirstPassWeight float64 `yaml:"ci_first_pass_weight" json:"ciFirstPassWeight"`
	ReworkWeight      float64 `yaml:"rework_weight" json:"reworkWeight"`
	FailureWeight     float64 `yaml:"failure_weight" json:"failureWeight"`
	// CostWeight and DurationWeight scale a normalized (0..1-clamped) cost/
	// duration penalty — see CostNormalizer/DurationNormalizer — so cost only
	// ever lowers a variant's rank, never zeroes its weight (FloorWeight is
	// the hard floor no penalty can cross).
	CostWeight         float64 `yaml:"cost_weight" json:"costWeight"`
	DurationWeight     float64 `yaml:"duration_weight" json:"durationWeight"`
	CostNormalizer     float64 `yaml:"cost_normalizer" json:"costNormalizer"`
	DurationNormalizer float64 `yaml:"duration_normalizer" json:"durationNormalizer"`
}
