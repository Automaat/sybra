package routing

import "sort"

const (
	defaultFloorWeight        = 1
	defaultMaxStep            = 5
	defaultWeightBudgetPerVar = 10 // used only when opts.WeightBudget <= 0
)

// withDefaults fills FloorWeight/MaxStep/WeightBudget when unset (<=0).
// MinSamplesToShift is left as given: <=0 means "no sample-size gate" (every
// scored, non-insufficient variant is rank-eligible), which is the correct
// zero-value behavior for direct callers of this pure function. The config
// layer (applyRoutingDefaults) fills a real positive default before this
// ever runs in production.
func (o PlanOptions) withDefaults(variantsPerExperiment int) PlanOptions {
	if o.FloorWeight <= 0 {
		o.FloorWeight = defaultFloorWeight
	}
	if o.MaxStep <= 0 {
		o.MaxStep = defaultMaxStep
	}
	if o.WeightBudget <= 0 {
		o.WeightBudget = variantsPerExperiment * defaultWeightBudgetPerVar
	}
	return o
}

// PlanWeights turns per-variant scores into a bounded weight plan.
//
// current is the full universe of configured (experimentID -> variantID ->
// weight) — including variants ScoreVariants never saw (no runs yet) — so
// every configured variant gets a plan entry. A variant absent from scores,
// or present but InsufficientData/SkillParityUnknown, or below
// MinSamplesToShift resolved runs, is exploration-only: it is never ranked
// and always lands at FloorWeight. Every other variant is ranked by Score
// descending and the remaining budget above the floor is distributed by
// rank, so a lower (or penalized) score can only ever lose relative share,
// never be pushed below the floor.
//
// Each variant's weight moves at most opts.MaxStep per call, in either
// direction, from its current value — bounding how much traffic one
// generation can re-route. PlanWeights is pure: no I/O, deterministic for a
// given input, and Changed is false (Experiments empty) when nothing would
// move.
func PlanWeights(scores []Score, current map[string]map[string]int, opts PlanOptions) WeightPlan {
	byExp := map[string][]Score{}
	for _, s := range scores {
		byExp[s.ExperimentID] = append(byExp[s.ExperimentID], s)
	}

	expIDs := make([]string, 0, len(current))
	for expID := range current {
		expIDs = append(expIDs, expID)
	}
	for expID := range byExp {
		if _, ok := current[expID]; !ok {
			expIDs = append(expIDs, expID)
		}
	}
	sort.Strings(expIDs)

	plan := WeightPlan{Experiments: map[string]map[string]int{}}
	for _, expID := range expIDs {
		final, changed := planExperiment(current[expID], byExp[expID], opts)
		if changed {
			plan.Experiments[expID] = final
			plan.Changed = true
		}
	}
	return plan
}

// planExperiment runs PlanWeights' floor/rank/bounded-step algorithm for one
// experiment's variants, returning the final weights and whether any moved.
func planExperiment(curWeights map[string]int, variantScores []Score, opts PlanOptions) (map[string]int, bool) {
	variantSet := map[string]bool{}
	for vid := range curWeights {
		variantSet[vid] = true
	}
	for _, s := range variantScores {
		variantSet[s.VariantID] = true
	}
	if len(variantSet) == 0 {
		return nil, false
	}
	variantIDs := make([]string, 0, len(variantSet))
	for vid := range variantSet {
		variantIDs = append(variantIDs, vid)
	}
	sort.Strings(variantIDs)

	o := opts.withDefaults(len(variantIDs))

	scoreByVariant := map[string]Score{}
	for _, s := range variantScores {
		scoreByVariant[s.VariantID] = s
	}
	eligible := rankEligibleVariants(variantIDs, scoreByVariant, o.MinSamplesToShift)
	target := distributeBudget(variantIDs, eligible, o)

	final := map[string]int{}
	changed := false
	for _, vid := range variantIDs {
		cur, ok := curWeights[vid]
		if !ok || cur <= 0 {
			cur = o.FloorWeight
		}
		delta := min(target[vid]-cur, o.MaxStep)
		delta = max(delta, -o.MaxStep)
		next := max(cur+delta, o.FloorWeight)
		final[vid] = next
		if next != cur {
			changed = true
		}
	}
	return final, changed
}

type rankedVariant struct {
	id    string
	value float64
}

// rankEligibleVariants selects variants sufficiently sampled to be ranked by
// score, sorted descending (deterministic tie-break by ID). A variant with no
// score at all, or InsufficientData/SkillParityUnknown, or below
// minSamplesToShift resolved runs, is never eligible — it stays
// exploration-only at the floor (see distributeBudget).
func rankEligibleVariants(variantIDs []string, scoreByVariant map[string]Score, minSamplesToShift int) []rankedVariant {
	var eligible []rankedVariant
	for _, vid := range variantIDs {
		s, ok := scoreByVariant[vid]
		sufficient := ok && !s.InsufficientData && !s.SkillParityUnknown &&
			(minSamplesToShift <= 0 || s.ResolvedRuns >= minSamplesToShift)
		if sufficient {
			eligible = append(eligible, rankedVariant{id: vid, value: s.Value})
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].value != eligible[j].value {
			return eligible[i].value > eligible[j].value
		}
		return eligible[i].id < eligible[j].id
	})
	return eligible
}

// distributeBudget assigns every variant the floor weight, then hands the
// remaining budget (WeightBudget minus every variant's floor) to eligible
// variants by rank — the top-ranked variant gets the largest share, down to
// the lowest-ranked eligible variant, so a lower score only ever loses
// relative share and never drops a variant below the floor.
func distributeBudget(variantIDs []string, eligible []rankedVariant, o PlanOptions) map[string]int {
	target := map[string]int{}
	for _, vid := range variantIDs {
		target[vid] = o.FloorWeight
	}
	remaining := o.WeightBudget - o.FloorWeight*len(variantIDs)
	if remaining <= 0 || len(eligible) == 0 {
		return target
	}
	totalRankWeight := len(eligible) * (len(eligible) + 1) / 2
	for i, r := range eligible {
		rank := len(eligible) - i // top-ranked gets the largest share
		target[r.id] += remaining * rank / totalRankWeight
	}
	return target
}
