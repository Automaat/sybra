package evaluation

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
)

func landedEvent(tid string, ts time.Time, outcome string) audit.Event {
	return audit.Event{Type: audit.EventTaskLanded, TaskID: tid, Timestamp: ts, Data: map[string]any{"outcome": outcome}}
}

func humanRequiredEvent(tid string, ts time.Time) audit.Event {
	return audit.Event{Type: audit.EventTaskStatusChanged, TaskID: tid, Timestamp: ts,
		Data: map[string]any{"from": "in-review", "to": "human-required"}}
}

// TestComputeAutonomyTrend_WeeklyBucketsPartition pins down that adjacent
// weekly buckets share a boundary instant (one bucket's weekEnd equals the
// next's weekStart) without double-counting a landing timestamped exactly on
// it — Compute's window is inclusive on both ends, so this only holds because
// ComputeAutonomyTrend trims a nanosecond off every bucket but the newest.
func TestComputeAutonomyTrend_WeeklyBucketsPartition(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	boundary := now.AddDate(0, 0, -14) // weekly[2]'s weekEnd == weekly[1]'s weekStart

	events := []audit.Event{
		landedEvent("on-boundary", boundary, "merged"),                                // must land in the newer bucket only
		landedEvent("just-before-boundary", boundary.Add(-time.Nanosecond), "merged"), // older bucket only
		landedEvent("recent", now.AddDate(0, 0, -3), "merged"),                        // newest bucket
	}

	got := ComputeAutonomyTrend(nil, events, now, 3)
	if len(got.Weekly) != 3 {
		t.Fatalf("len(Weekly) = %d, want 3", len(got.Weekly))
	}
	oldest, middle, newest := got.Weekly[0], got.Weekly[1], got.Weekly[2]

	if oldest.TasksLanded != 1 {
		t.Errorf("oldest bucket TasksLanded = %d, want 1 (just-before-boundary)", oldest.TasksLanded)
	}
	if middle.TasksLanded != 1 {
		t.Errorf("middle bucket TasksLanded = %d, want 1 (on-boundary)", middle.TasksLanded)
	}
	if newest.TasksLanded != 1 {
		t.Errorf("newest bucket TasksLanded = %d, want 1 (recent)", newest.TasksLanded)
	}
	total := oldest.TasksLanded + middle.TasksLanded + newest.TasksLanded
	if total != 3 {
		t.Errorf("total TasksLanded across buckets = %d, want 3 (no double-count on the shared boundary)", total)
	}
}

// TestComputeAutonomyTrend_OverallIsWiderThanRecentWindows proves Overall
// actually reads full history rather than just relabeling LastMonth: a
// landing far older than both the 30-day LastMonth window and the default
// 12-week weekly horizon must appear in Overall and nowhere else. Also pins
// the weeks<=0 default-to-12 fallback.
func TestComputeAutonomyTrend_OverallIsWiderThanRecentWindows(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	events := []audit.Event{
		landedEvent("ancient", now.AddDate(0, 0, -200), "merged"),
	}

	got := ComputeAutonomyTrend(nil, events, now, 0)

	if len(got.Weekly) != autonomyTrendWeeks {
		t.Fatalf("len(Weekly) = %d, want %d (weeks<=0 should default)", len(got.Weekly), autonomyTrendWeeks)
	}
	if got.Overall.TasksLanded != 1 {
		t.Errorf("Overall.TasksLanded = %d, want 1", got.Overall.TasksLanded)
	}
	if got.LastMonth.TasksLanded != 0 {
		t.Errorf("LastMonth.TasksLanded = %d, want 0 (landing predates the 30-day window)", got.LastMonth.TasksLanded)
	}
	if got.LastWeek.TasksLanded != 0 {
		t.Errorf("LastWeek.TasksLanded = %d, want 0", got.LastWeek.TasksLanded)
	}
	weeklyTotal := 0
	for _, w := range got.Weekly {
		weeklyTotal += w.TasksLanded
	}
	if weeklyTotal != 0 {
		t.Errorf("sum(Weekly.TasksLanded) = %d, want 0 (landing predates the weekly history horizon)", weeklyTotal)
	}
}

// TestComputeAutonomyTrend_SnapshotsMatchDirectCompute checks
// ComputeAutonomyTrend windows autonomy rather than reimplementing it: its
// LastWeek/LastMonth snapshots must agree exactly with a direct Compute()
// call over the same bounds.
func TestComputeAutonomyTrend_SnapshotsMatchDirectCompute(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	events := []audit.Event{
		landedEvent("autonomous", now.AddDate(0, 0, -2), "merged"),
		landedEvent("human-touched", now.AddDate(0, 0, -5), "merged"),
		humanRequiredEvent("human-touched", now.AddDate(0, 0, -6)),
		landedEvent("older-in-month", now.AddDate(0, 0, -20), "merged"),
	}
	records := []stats.RunRecord{
		{TaskID: "autonomous", Outcome: "completed", Timestamp: now.AddDate(0, 0, -2)},
	}

	trend := ComputeAutonomyTrend(records, events, now, 4)
	wantLastWeek := Compute(records, events, now.AddDate(0, 0, -7), now)
	wantLastMonth := Compute(records, events, now.AddDate(0, 0, -30), now)

	if trend.LastWeek.TasksLanded != wantLastWeek.TasksLanded || trend.LastWeek.AutonomousLandings != wantLastWeek.AutonomousLandings || trend.LastWeek.AutonomyRate != wantLastWeek.AutonomyRate {
		t.Errorf("LastWeek = %+v, want landed=%d autonomous=%d rate=%v", trend.LastWeek, wantLastWeek.TasksLanded, wantLastWeek.AutonomousLandings, wantLastWeek.AutonomyRate)
	}
	if trend.LastMonth.TasksLanded != wantLastMonth.TasksLanded || trend.LastMonth.AutonomousLandings != wantLastMonth.AutonomousLandings || trend.LastMonth.AutonomyRate != wantLastMonth.AutonomyRate {
		t.Errorf("LastMonth = %+v, want landed=%d autonomous=%d rate=%v", trend.LastMonth, wantLastMonth.TasksLanded, wantLastMonth.AutonomousLandings, wantLastMonth.AutonomyRate)
	}
	if trend.LastMonth.TasksLanded != 3 {
		t.Errorf("LastMonth.TasksLanded = %d, want 3 (autonomous, human-touched, older-in-month)", trend.LastMonth.TasksLanded)
	}
	if trend.LastMonth.AutonomousLandings != 2 {
		t.Errorf("LastMonth.AutonomousLandings = %d, want 2", trend.LastMonth.AutonomousLandings)
	}
}
