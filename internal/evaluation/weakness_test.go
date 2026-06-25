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
