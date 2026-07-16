package evaluation

import (
	"strings"
	"testing"
)

func hasMetric(ws []Weakness, metric string) bool {
	for _, w := range ws {
		if w.Metric == metric {
			return true
		}
	}
	return false
}

func TestWeaknesses_FlagsShortfalls(t *testing.T) {
	r := Report{
		Overall: Scorecard{
			TasksLanded:     10,
			AutonomyRate:    0.4, // < 0.7 → autonomy
			CIFirstPassRate: 0.5, // < 0.6 → ci_first_pass
			ReworkTasks:     5,   // 5/10 = 0.5 > 0.3 → rework
			AgentRuns:       20,
			FailureRate:     0.3, // > 0.2 → failure_rate
		},
		ByProvider: []Breakdown{
			{Key: "codex", Runs: 10, FailureRate: 0.6}, // 0.6 > 0.3+0.15 → outlier
			{Key: "claude", Runs: 10, FailureRate: 0.3},
		},
	}
	ws := Weaknesses(r)
	for _, m := range []string{"autonomy", "ci_first_pass", "rework", "failure_rate", "provider:codex"} {
		if !hasMetric(ws, m) {
			t.Errorf("expected weakness %q, got %+v", m, ws)
		}
	}
	if hasMetric(ws, "provider:claude") {
		t.Errorf("claude is not an outlier; should not be flagged")
	}
	// Ranked: all warn-severity weaknesses precede any info-severity ones.
	seenInfo := false
	for _, w := range ws {
		if w.Severity == "info" {
			seenInfo = true
		} else if w.Severity == "warn" && seenInfo {
			t.Errorf("unranked: warn %q appears after an info weakness", w.Metric)
		}
	}
}

func TestWeaknesses_HealthyAndLowData(t *testing.T) {
	// Healthy fleet with enough data → no weaknesses.
	healthy := Report{Overall: Scorecard{
		TasksLanded: 10, AutonomyRate: 0.95, CIFirstPassRate: 0.9, ReworkTasks: 0,
		AgentRuns: 20, FailureRate: 0.05,
	}}
	if ws := Weaknesses(healthy); len(ws) != 0 {
		t.Errorf("healthy fleet flagged: %+v", ws)
	}

	// Too little data → gates suppress everything despite bad ratios.
	sparse := Report{Overall: Scorecard{
		TasksLanded: 1, AutonomyRate: 0, CIFirstPassRate: 0, ReworkTasks: 1,
		AgentRuns: 1, FailureRate: 1,
	}}
	if ws := Weaknesses(sparse); len(ws) != 0 {
		t.Errorf("sparse window flagged (should be gated): %+v", ws)
	}
}

// Weaknesses tell the operator to go fix a prompt or route work away from a
// provider. A window of stalls is no evidence for either, so a role/provider
// whose runs mostly stalled must not clear the signal gate on the strength of
// its dispatch count (#2149).
func TestWeaknesses_GateCountsResolvedRunsNotStalls(t *testing.T) {
	r := Report{
		Overall: Scorecard{
			// 20 dispatches but only 4 resolved: below minRunsForSignal.
			AgentRuns: 20, AgentStalls: 16, FailureRate: 1,
		},
		ByProvider: []Breakdown{
			// 10 dispatches, 1 resolved, and it failed: a sample of one.
			{Key: "codex", Runs: 10, Stalled: 9, Failures: 1, FailureRate: 1},
		},
	}
	if ws := Weaknesses(r); len(ws) != 0 {
		t.Errorf("Weaknesses = %+v, want none: 4 resolved fleet runs and 1 resolved codex run are below the signal gate", ws)
	}
}

// Stalls alongside a sufficient number of resolved runs neither block the gate
// nor change the rate the weakness narrates.
func TestWeaknesses_StallsDoNotBlockAGenuineSignal(t *testing.T) {
	r := Report{
		Overall: Scorecard{AgentRuns: 40, AgentStalls: 20, AgentFailures: 6, FailureRate: 0.3},
		ByProvider: []Breakdown{
			{Key: "codex", Runs: 20, Stalled: 10, Failures: 6, FailureRate: 0.6},
		},
	}
	ws := Weaknesses(r)
	if !hasMetric(ws, "failure_rate") || !hasMetric(ws, "provider:codex") {
		t.Errorf("Weaknesses = %+v, want failure_rate and provider:codex flagged", ws)
	}
	for _, w := range ws {
		if w.Metric == "failure_rate" && !strings.Contains(w.Detail, "resolved") {
			t.Errorf("failure_rate detail = %q, want it to name the resolved-run basis", w.Detail)
		}
	}
}

func TestWeaknesses_SuggestionsPresent(t *testing.T) {
	r := Report{Overall: Scorecard{TasksLanded: 10, AutonomyRate: 0.1, CIFirstPassRate: 1, AgentRuns: 10}}
	ws := Weaknesses(r)
	if len(ws) == 0 {
		t.Fatal("expected at least one weakness")
	}
	for _, w := range ws {
		if strings.TrimSpace(w.Suggestion) == "" {
			t.Errorf("weakness %q has no suggestion", w.Metric)
		}
	}
}
