package evaluation

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
)

func TestCompute(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour) // inside window
	out := base.AddDate(0, 0, -40) // outside window
	ld := func(tid string, ts time.Time, data map[string]any) audit.Event {
		return audit.Event{Type: audit.EventTaskLanded, TaskID: tid, Timestamp: ts, Data: data}
	}
	sc := func(tid, from, to string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventTaskStatusChanged, TaskID: tid, Timestamp: ts,
			Data: map[string]any{"from": from, "to": to}}
	}

	preWindow := since.Add(-1 * time.Hour) // before the landing window, within signal range
	events := []audit.Event{
		// Task A: merged, clean (no human, no ci-fix, no rework).
		ld("A", in, map[string]any{"outcome": "merged", "created_to_land_h": 10.0, "work_to_land_h": 4.0}),
		// Task B: closed, with a repeated transition (rework) in-window, plus a
		// human-required escalation and CI fix that happened BEFORE the landing
		// window — these must still count (straddle), proving signals are not
		// window-bound.
		ld("B", in, map[string]any{"outcome": "closed", "created_to_land_h": 20.0, "work_to_land_h": 8.0}),
		sc("B", "in-review", "human-required", preWindow),
		sc("B", "in-progress", "in-review", in),
		sc("B", "in-progress", "in-review", in), // repeat → rework
		{Type: audit.EventPRCIFailureDetected, TaskID: "B", Timestamp: preWindow},
		// Task D bounces but never lands → rework must NOT count it.
		sc("D", "in-progress", "in-review", in),
		sc("D", "in-progress", "in-review", in),
		// Out of window → ignored entirely.
		ld("C", out, map[string]any{"outcome": "merged", "created_to_land_h": 999.0}),
	}
	// Reliability + efficiency come from stats run records: 4 in-window runs,
	// 1 failed → failure rate 0.25. C is out of window → ignored.
	records := []stats.RunRecord{
		{TaskID: "A", CostUSD: 1.0, TurnCount: 5, ToolCalls: 10, Outcome: "completed", Timestamp: in},
		{TaskID: "B", CostUSD: 3.0, TurnCount: 15, ToolCalls: 30, Outcome: "completed", Timestamp: in},
		{TaskID: "B", CostUSD: 0, TurnCount: 0, ToolCalls: 0, Outcome: "completed", Timestamp: in},
		{TaskID: "B", CostUSD: 0, TurnCount: 0, ToolCalls: 0, Outcome: "failed", Timestamp: in},
		{TaskID: "C", CostUSD: 99.0, TurnCount: 99, ToolCalls: 99, Outcome: "failed", Timestamp: out}, // ignored
	}

	got := Compute(records, events, since, base)

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"TasksLanded", float64(got.TasksLanded), 2},
		{"Merged", float64(got.Merged), 1},
		{"Closed", float64(got.Closed), 1},
		{"MergeRate", got.MergeRate, 0.5},
		{"LeadTimeP50H", got.LeadTimeP50H, 10},
		{"LeadTimeP90H", got.LeadTimeP90H, 20},
		{"CycleTimeP50H", got.CycleTimeP50H, 4},
		{"CycleTimeP90H", got.CycleTimeP90H, 8},
		{"AutonomousLandings", float64(got.AutonomousLandings), 1},
		{"HumanTouchedLandings", float64(got.HumanTouchedLandings), 1},
		{"AutonomyRate", got.AutonomyRate, 0.5},
		{"AgentRuns", float64(got.AgentRuns), 4},
		{"AgentFailures", float64(got.AgentFailures), 1},
		{"FailureRate", got.FailureRate, 0.25},
		{"CIFirstPassRate", got.CIFirstPassRate, 0.5},
		{"TotalCostUSD", got.TotalCostUSD, 4.0},
		{"CostPerLanded", got.CostPerLanded, 2.0},
		{"TurnsPerLanded", got.TurnsPerLanded, 10},
		{"ToolsPerLanded", got.ToolsPerLanded, 20},
		{"ReworkTasks", float64(got.ReworkTasks), 1},
		{"WindowDays", got.WindowDays, 30},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestBreakdownBy(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	out := base.AddDate(0, 0, -30)
	records := []stats.RunRecord{
		{Provider: "claude", CostUSD: 1.0, TurnCount: 3, ToolCalls: 6, Outcome: "completed", Timestamp: in},
		{Provider: "claude", CostUSD: 2.0, TurnCount: 1, ToolCalls: 2, Outcome: "failed", Timestamp: in},
		{Provider: "codex", CostUSD: 5.0, TurnCount: 4, ToolCalls: 8, Outcome: "completed", Timestamp: in},
		{Provider: "claude", CostUSD: 9.0, Outcome: "failed", Timestamp: out}, // out of window
		{Provider: "", CostUSD: 1.0, Outcome: "completed", Timestamp: in},     // empty key skipped
	}
	got := BreakdownBy(records, since, base, func(r stats.RunRecord) string { return r.Provider })
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2 (claude, codex): %+v", len(got), got)
	}
	// Sorted by key: claude first.
	cl := got[0]
	if cl.Key != "claude" || cl.Runs != 2 || cl.Failures != 1 || cl.FailureRate != 0.5 || cl.TotalCostUSD != 3.0 || cl.Turns != 4 || cl.Tools != 8 {
		t.Errorf("claude breakdown = %+v", cl)
	}
	cx := got[1]
	if cx.Key != "codex" || cx.Runs != 1 || cx.Failures != 0 || cx.TotalCostUSD != 5.0 {
		t.Errorf("codex breakdown = %+v", cx)
	}
}

func TestCompute_MergedWithEditsNotAutonomous(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour)
	ld := func(tid, outcome string) audit.Event {
		return audit.Event{Type: audit.EventTaskLanded, TaskID: tid, Timestamp: in, Data: map[string]any{"outcome": outcome}}
	}
	events := []audit.Event{
		ld("A", "merged"),            // clean → autonomous
		ld("E", "merged_with_edits"), // human edited → not autonomous
	}
	got := Compute(nil, events, since, base)
	if got.TasksLanded != 2 {
		t.Fatalf("TasksLanded = %d, want 2", got.TasksLanded)
	}
	if got.Merged != 1 || got.MergedWithEdits != 1 {
		t.Errorf("Merged/MergedWithEdits = %d/%d, want 1/1", got.Merged, got.MergedWithEdits)
	}
	if got.AutonomousLandings != 1 {
		t.Errorf("AutonomousLandings = %d, want 1 (only the clean merge)", got.AutonomousLandings)
	}
	if got.HumanTouchedLandings != 1 {
		t.Errorf("HumanTouchedLandings = %d, want 1 (the edited merge)", got.HumanTouchedLandings)
	}
	if got.AutonomyRate != 0.5 {
		t.Errorf("AutonomyRate = %v, want 0.5", got.AutonomyRate)
	}
}

func TestCompute_ChangeFailureRate(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour)
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "B", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "C", Timestamp: in, Data: map[string]any{"outcome": "merged_with_edits"}},
		{Type: audit.EventPRReverted, TaskID: "A", Timestamp: in}, // one of 3 merged landings reverted
	}
	got := Compute(nil, events, since, base)
	if got.Reverted != 1 {
		t.Errorf("Reverted = %d, want 1", got.Reverted)
	}
	// 1 revert / 3 merged landings (merged + merged_with_edits).
	if got.ChangeFailureRate < 0.33 || got.ChangeFailureRate > 0.34 {
		t.Errorf("ChangeFailureRate = %v, want ~0.333", got.ChangeFailureRate)
	}
}

func TestComputeEmpty(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	got := Compute(nil, nil, now.AddDate(0, 0, -7), now)
	// No data must yield zeros, no division-by-zero, no NaN.
	if got.TasksLanded != 0 || got.MergeRate != 0 || got.AutonomyRate != 0 || got.FailureRate != 0 {
		t.Errorf("empty compute not zeroed: %+v", got)
	}
	if got.WindowDays != 7 {
		t.Errorf("WindowDays = %v, want 7", got.WindowDays)
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name string
		xs   []float64
		p    float64
		want float64
	}{
		{"empty", nil, 50, 0},
		{"single", []float64{5}, 50, 5},
		{"p0", []float64{3, 1, 2}, 0, 1},
		{"p100", []float64{3, 1, 2}, 100, 3},
		{"p50 of 4", []float64{1, 2, 3, 4}, 50, 2},
		{"p90 of 10", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 90, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.xs, tt.p); got != tt.want {
				t.Errorf("percentile(%v, %v) = %v, want %v", tt.xs, tt.p, got, tt.want)
			}
		})
	}
}
