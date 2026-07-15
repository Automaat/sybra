package promptlab

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/stats"
)

// repeatRecords returns n records for role with the given outcome, so tests
// can build up per-role run/failure counts without hand-listing each record.
func repeatRecords(n int, role, outcome, projectID, taskIDPrefix string) []stats.RunRecord {
	out := make([]stats.RunRecord, n)
	for i := range out {
		out[i] = stats.RunRecord{
			Role:      role,
			Outcome:   outcome,
			ProjectID: projectID,
			TaskID:    taskIDPrefix + string(rune('a'+i)),
		}
	}
	return out
}

func TestCollectWeakSubjectsGates(t *testing.T) {
	var records []stats.RunRecord
	// implementation: 10 runs, 5 failed -> 0.50 failure rate, clears both gates.
	records = append(records, repeatRecords(5, "implementation", "failed", "p1", "impl-fail-")...)
	records = append(records, repeatRecords(5, "implementation", "ok", "p2", "impl-ok-")...)
	// review: 10 runs, 1 failed -> 0.10 failure rate, below min effect size.
	records = append(records, repeatRecords(1, "review", "failed", "p1", "review-fail-")...)
	records = append(records, repeatRecords(9, "review", "ok", "p1", "review-ok-")...)
	// fix-review: 2 runs, both failed -> below min samples despite high rate.
	records = append(records, repeatRecords(2, "fix-review", "failed", "p1", "fix-review-")...)
	// docs: 10 runs, all passing -> dilutes the fleet baseline to 0.25.
	records = append(records, repeatRecords(10, "docs", "ok", "p1", "docs-")...)

	got := CollectWeakSubjects(records, 5, 0.15)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1: %+v", len(got), got)
	}
	if got[0].Role != "implementation" {
		t.Fatalf("role = %q, want implementation", got[0].Role)
	}
	if got[0].Samples != 10 {
		t.Fatalf("samples = %d, want 10", got[0].Samples)
	}
	wantEffect := 0.25 // 0.50 role rate - 0.25 overall rate
	if diff := got[0].EffectSize - wantEffect; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("effect size = %v, want %v", got[0].EffectSize, wantEffect)
	}
	if len(got[0].ProjectIDs) != 2 {
		t.Fatalf("projectIds = %v, want 2 entries", got[0].ProjectIDs)
	}
}

func TestCollectWeakSubjectsNoRecords(t *testing.T) {
	got := CollectWeakSubjects(nil, 5, 0.15)
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
