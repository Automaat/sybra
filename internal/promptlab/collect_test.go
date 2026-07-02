package promptlab

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/stats"
)

func TestCollectWeakSubjectsGates(t *testing.T) {
	report := evaluation.Report{
		Overall: evaluation.Scorecard{FailureRate: 0.10},
		ByRole: []evaluation.Breakdown{
			{Key: "implementation", Runs: 20, FailureRate: 0.40}, // clears both gates
			{Key: "review", Runs: 20, FailureRate: 0.15},         // below min effect size
			{Key: "fix-review", Runs: 2, FailureRate: 0.90},      // below min samples
		},
	}
	records := []stats.RunRecord{
		{Role: "implementation", ProjectID: "p1", TaskID: "t1"},
		{Role: "implementation", ProjectID: "p2", TaskID: "t2"},
	}

	got := CollectWeakSubjects(report, records, 10, 0.15)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	if got[0].Role != "implementation" {
		t.Fatalf("role = %q, want implementation", got[0].Role)
	}
	if got[0].Samples != 20 {
		t.Fatalf("samples = %d, want 20", got[0].Samples)
	}
	wantEffect := 0.30
	if diff := got[0].EffectSize - wantEffect; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("effect size = %v, want %v", got[0].EffectSize, wantEffect)
	}
	if len(got[0].ProjectIDs) != 2 {
		t.Fatalf("projectIds = %v, want 2 entries", got[0].ProjectIDs)
	}
}

func TestCollectWeakSubjectsEmptyReport(t *testing.T) {
	got := CollectWeakSubjects(evaluation.Report{}, nil, 5, 0.15)
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

func TestWithinLookback(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	records := []stats.RunRecord{
		{TaskID: "old", Timestamp: now.Add(-30 * 24 * time.Hour)},
		{TaskID: "recent", Timestamp: now.Add(-1 * time.Hour)},
	}
	got := withinLookback(records, 7*24*time.Hour, now)
	if len(got) != 1 || got[0].TaskID != "recent" {
		t.Fatalf("withinLookback = %+v, want only 'recent'", got)
	}
	if all := withinLookback(records, 0, now); len(all) != 2 {
		t.Fatalf("zero lookback should disable filtering, got %d records", len(all))
	}
}
