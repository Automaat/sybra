package routing

import (
	"sort"

	"github.com/Automaat/sybra/internal/evaluation"
)

// ScoreVariants derives a composite outcome score per (experimentID,
// variantID) from evaluation.ComparisonBreakdown rows — the same rows the
// Evaluation tab renders, with skill-parity/infra-stall exclusion already
// applied. Pure and deterministic: rows carrying no experiment/variant
// attribution are skipped, and the result is sorted for stable output.
//
// Gains (landed/merge/CI-first-pass) use the Wilson lower bound so a lucky
// small sample cannot outrank a well-sampled variant; penalties (rework/
// failure) use the Wilson upper bound so an unlucky small sample is not
// under-penalized. Cost and duration are normalized into a 0..1-clamped
// range before their coefficient is applied, so a single outlier run cannot
// dominate the composite score.
func ScoreVariants(rows []evaluation.ComparisonBreakdown, coef Coefficients) []Score {
	scores := make([]Score, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		if r.ExperimentID == "" || r.VariantID == "" {
			continue
		}
		inputs := ScoreInputs{
			LandedWilsonLower:      r.LandedEstimate.WilsonLower,
			MergeWilsonLower:       r.MergeEstimate.WilsonLower,
			CIFirstPassWilsonLower: r.CIFirstPassEstimate.WilsonLower,
			ReworkWilsonUpper:      r.ReworkEstimate.WilsonUpper,
			FailureWilsonUpper:     r.FailureEstimate.WilsonUpper,
			CostPerLanded:          r.CostPerLanded,
			DurationP50S:           r.DurationP50S,
		}
		value := coef.LandedWeight*inputs.LandedWilsonLower +
			coef.MergeWeight*inputs.MergeWilsonLower +
			coef.CIFirstPassWeight*inputs.CIFirstPassWilsonLower -
			coef.ReworkWeight*inputs.ReworkWilsonUpper -
			coef.FailureWeight*inputs.FailureWilsonUpper -
			coef.CostWeight*normalize(inputs.CostPerLanded, coef.CostNormalizer) -
			coef.DurationWeight*normalize(inputs.DurationP50S, coef.DurationNormalizer)

		scores = append(scores, Score{
			ExperimentID:       r.ExperimentID,
			VariantID:          r.VariantID,
			Value:              value,
			Runs:               r.Runs,
			ResolvedRuns:       r.ResolvedRuns,
			InsufficientData:   r.InsufficientData,
			SkillParityUnknown: r.SkillParityUnknown,
			Inputs:             inputs,
		})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].ExperimentID != scores[j].ExperimentID {
			return scores[i].ExperimentID < scores[j].ExperimentID
		}
		return scores[i].VariantID < scores[j].VariantID
	})
	return scores
}

// normalize clamps v/scale to [0, 1]. scale <= 0 disables the term
// (returns 0) rather than dividing by zero, so an unconfigured normalizer
// silently drops that penalty instead of producing Inf/NaN.
func normalize(v, scale float64) float64 {
	if scale <= 0 || v <= 0 {
		return 0
	}
	n := v / scale
	if n > 1 {
		return 1
	}
	return n
}
