package evaluation

import (
	"fmt"
	"sort"
)

// Weakness is one systematic shortfall the scorecard surfaces, paired with a
// concrete suggested action — the deterministic half of the improvement loop.
type Weakness struct {
	Severity   string `json:"severity"` // "warn" | "info"
	Metric     string `json:"metric"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// Signal gates: don't emit weaknesses from a near-empty window, where a single
// task or run swings every ratio. Run gates count resolved runs, never
// dispatches — a window of stalls carries no evidence about prompts or
// guardrails, which is what these weaknesses tell the operator to go fix.
const (
	minLandedForSignal = 3
	minRunsForSignal   = 5
	// outlierMargin is how far a provider/role failure rate must exceed the
	// overall before it's flagged as a systematic outlier.
	outlierMargin = 0.15
	// minMergedForSignal gates the cost-per-merge regression check: both the
	// current and prior window must have landed at least this many merges
	// (Merged+MergedWithEdits), or a handful of merges could swing $/merged
	// wildly and flag a false regression.
	minMergedForSignal = 5
	// costRegressionMargin is how far $/merged-PR must rise relative to the
	// prior equal-length window before it's flagged as a regression.
	costRegressionMargin = 0.20
)

// Weaknesses inspects a report and returns ranked systematic weaknesses, each
// with a suggested action. Empty when there is too little data or nothing
// stands out. targets gates the autonomy/CI-first-pass/rework thresholds —
// pass config.DefaultSLOTargets() (or the operator's EvaluationConfig.SLO)
// rather than a hardcoded value so a fleet-wide SLO change and a weakness
// warning stay in lockstep. Pure and deterministic.
func Weaknesses(r Report, targets SLOTargets) []Weakness {
	var out []Weakness
	o := r.Overall

	if o.TasksLanded >= minLandedForSignal {
		if o.AutonomyRate < targets.MinAutonomyRate {
			out = append(out, Weakness{
				Severity: "warn",
				Metric:   "autonomy",
				// AutonomyRate is scoped to the known-provenance cohort
				// (autonomous + humanTouched) — AutonomyUnknownLandings is
				// excluded from both buckets rather than guessed into one
				// (issue #2727), so 1-AutonomyRate here is purely the
				// human-touched share of the known cohort.
				Detail:     fmt.Sprintf("%.0f%% of landings with known provenance were human-touched", (1-o.AutonomyRate)*100),
				Suggestion: "review what escalated to human-required; tighten triage so more tasks run headless end-to-end",
			})
		}
		if o.CIFirstPassRate < targets.MinCIFirstPassRate {
			out = append(out, Weakness{
				Severity:   "warn",
				Metric:     "ci_first_pass",
				Detail:     fmt.Sprintf("only %.0f%% of landings passed CI on the first push", o.CIFirstPassRate*100),
				Suggestion: "have the implementation agent run the full test/lint suite before pushing",
			})
		}
		if rework := float64(o.ReworkTasks) / float64(o.TasksLanded); rework > targets.MaxReworkRate {
			out = append(out, Weakness{
				Severity:   "info",
				Metric:     "rework",
				Detail:     fmt.Sprintf("%.0f%% of landed tasks bounced between statuses", rework*100),
				Suggestion: "strengthen plan-review so tasks converge in fewer rounds",
			})
		}
	}

	if mergedNow := o.Merged + o.MergedWithEdits; mergedNow >= minMergedForSignal &&
		r.CostPerMergedBaseline != nil && r.CostPerMergedBaseline.MergedPRs >= minMergedForSignal &&
		r.CostPerMergedBaseline.CostPerMergedUSD > 0 {
		delta := (o.CostPerMergedUSD - r.CostPerMergedBaseline.CostPerMergedUSD) / r.CostPerMergedBaseline.CostPerMergedUSD
		if delta > costRegressionMargin {
			out = append(out, Weakness{
				Severity: "warn",
				Metric:   "cost_per_merge",
				Detail: fmt.Sprintf("$/merged-PR rose %.0f%% to $%.2f (was $%.2f last window)",
					delta*100, o.CostPerMergedUSD, r.CostPerMergedBaseline.CostPerMergedUSD),
				Suggestion: "check byCostTier for a tier/model regression or runaway retries inflating cost per merged PR",
			})
		}
	}

	if o.AgentResolvedRuns >= minRunsForSignal && o.FailureRate > 0.2 {
		out = append(out, Weakness{
			Severity:   "warn",
			Metric:     "failure_rate",
			Detail:     fmt.Sprintf("%.0f%% of resolved agent runs failed", o.FailureRate*100),
			Suggestion: "investigate the failing roles/providers below; check guardrails and prompts",
		})
	}

	out = append(out, outlierWeaknesses(r.ByProvider, o.FailureRate, "provider")...)
	out = append(out, outlierWeaknesses(r.ByRole, o.FailureRate, "role")...)

	// Rank by severity (warn before info), stable within a severity so the
	// detection order is preserved for equal-severity entries.
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

func severityRank(sev string) int {
	switch sev {
	case "warn":
		return 0
	case "info":
		return 1
	default:
		return 2
	}
}

// outlierWeaknesses flags any breakdown group whose failure rate exceeds the
// overall by more than outlierMargin, given enough runs.
func outlierWeaknesses(groups []Breakdown, overallFailure float64, dimension string) []Weakness {
	var out []Weakness
	for i := range groups {
		b := groups[i]
		if b.ResolvedRuns < minRunsForSignal || b.FailureRate <= overallFailure+outlierMargin {
			continue
		}
		out = append(out, Weakness{
			Severity:   "info",
			Metric:     dimension + ":" + b.Key,
			Detail:     fmt.Sprintf("%s %s fails %.0f%% vs %.0f%% overall", dimension, b.Key, b.FailureRate*100, overallFailure*100),
			Suggestion: fmt.Sprintf("review the %s %s prompt/guardrails or route work away from it", dimension, b.Key),
		})
	}
	return out
}
