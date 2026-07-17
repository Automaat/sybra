package evaluation

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/skillattr"
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

// #2149: a stalled run is retried, not resolved, so it must stay out of both
// sides of every failure rate. Codex stalls on ~96% of implementation runs, so
// leaving stalls in the denominator would rank the stall-prone provider as the
// most reliable one — the same corrupted evidence Prompt Lab gates on.
func TestComputeExcludesStallsFromFailureRate(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", CostUSD: 1.0, Outcome: stats.OutcomeCompleted, Timestamp: in},
		{TaskID: "A", CostUSD: 1.0, Outcome: stats.OutcomeFailed, Timestamp: in},
		{TaskID: "A", CostUSD: 0, Outcome: stats.OutcomeStalled, Timestamp: in},
		{TaskID: "A", CostUSD: 0, Outcome: stats.OutcomeStalled, Timestamp: in},
		{TaskID: "A", CostUSD: 0, Outcome: stats.OutcomeStalled, Timestamp: in},
	}

	got := Compute(records, nil, since, base)

	if got.AgentRuns != 5 {
		t.Errorf("AgentRuns = %d, want 5 (stalls burn real wall-clock, so they stay counted)", got.AgentRuns)
	}
	if got.AgentStalls != 3 {
		t.Errorf("AgentStalls = %d, want 3", got.AgentStalls)
	}
	if got.AgentFailures != 1 {
		t.Errorf("AgentFailures = %d, want 1", got.AgentFailures)
	}
	if got.FailureRate != 0.5 {
		t.Errorf("FailureRate = %v, want 0.5 (1 failure over 2 resolved runs, not 5 dispatched)", got.FailureRate)
	}
}

func TestBreakdownByExcludesStallsFromFailureRate(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		// codex: stalls constantly, and every run that resolved failed.
		{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeFailed, Timestamp: in},
		// claude: never stalls, resolves half its runs into failures.
		{Provider: "claude", Outcome: stats.OutcomeCompleted, Timestamp: in},
		{Provider: "claude", Outcome: stats.OutcomeFailed, Timestamp: in},
	}

	got := BreakdownBy(records, since, base, func(r stats.RunRecord) string { return r.Provider })
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(got), got)
	}
	cl, cx := got[0], got[1]
	if cl.Key != "claude" || cl.Stalled != 0 || cl.FailureRate != 0.5 {
		t.Errorf("claude breakdown = %+v, want stalled=0 failureRate=0.5", cl)
	}
	if cx.Key != "codex" || cx.Runs != 4 || cx.Stalled != 3 || cx.Failures != 1 {
		t.Errorf("codex breakdown = %+v, want runs=4 stalled=3 failures=1", cx)
	}
	if cx.FailureRate != 1.0 {
		t.Errorf("codex FailureRate = %v, want 1.0: its one resolved run failed. Counting the 3 stalls in the denominator would report 0.25 and rank the stall-prone provider above claude", cx.FailureRate)
	}
}

func TestCompareByExcludesStallsFromFailureEstimate(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeFailed, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeCompleted, Timestamp: in},
	}

	rows := CompareByLatestAuthor(records, nil, since, base, 0, func(r stats.RunRecord) string { return r.Provider })
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Runs != 4 || row.Stalled != 2 {
		t.Errorf("row = %+v, want runs=4 stalled=2", row)
	}
	if row.FailureEstimate.Denominator != 2 {
		t.Errorf("FailureEstimate.Denominator = %d, want 2 (resolved runs only)", row.FailureEstimate.Denominator)
	}
	if row.FailureRate != 0.5 {
		t.Errorf("FailureRate = %v, want 0.5", row.FailureRate)
	}
}

// The failure rate and the check on whether it has enough samples must count
// the same population. Gating on dispatches while rating over resolved runs
// lets a row declare itself actionable at a 100% failure rate off n=1.
func TestCompareByGatesSamplesOnResolvedRuns(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := make([]stats.RunRecord, 0, 30)
	for range 29 {
		records = append(records, stats.RunRecord{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in})
	}
	records = append(records, stats.RunRecord{Provider: "codex", Outcome: stats.OutcomeFailed, Timestamp: in})

	rows := CompareByLatestAuthor(records, nil, since, base, 30, func(r stats.RunRecord) string { return r.Provider })
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if !rows[0].InsufficientData {
		t.Errorf("InsufficientData = false for a row with 1 resolved run against minSamples=30 (runs=%d stalled=%d failureRate=%v)",
			rows[0].Runs, rows[0].Stalled, rows[0].FailureRate)
	}
}

// Stall records only exist going forward, so a window straddling the upgrade
// shows an inflated failure rate next to a stall count of ~0 — which reads as
// if the fix never landed. The report has to say so itself; the operator
// looking at the dashboard is the one who needs to know.
func TestReportNotesFlagsPreFixStallsRecordedAsFailures(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		// Pre-fix stalls: recorded as failures, and a stall's usage event never
		// arrived, so they cost nothing and produced nothing.
		{Outcome: stats.OutcomeFailed, Timestamp: in},
		{Outcome: stats.OutcomeFailed, Timestamp: in},
		// A genuine failure that actually did work.
		{Outcome: stats.OutcomeFailed, CostUSD: 0.76, OutputTokens: 900, Timestamp: in},
		{Outcome: stats.OutcomeCompleted, CostUSD: 1.2, OutputTokens: 500, Timestamp: in},
	}

	notes := reportNotes(records, since, base)
	found := ""
	for _, n := range notes {
		if strings.Contains(n, "zero cost and zero tokens") {
			found = n
		}
	}
	if found == "" {
		t.Fatalf("reportNotes = %v, want a note about failed runs that recorded no cost or tokens", notes)
	}
	if !strings.Contains(found, "2 of 3") {
		t.Errorf("note = %q, want it to count 2 of 3 failed runs", found)
	}
}

// Once the window is clear of pre-fix records the note must disappear, or it
// becomes permanent furniture that no one reads.
func TestReportNotesOmitsStallCaveatWhenFailuresAreAccounted(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{Outcome: stats.OutcomeFailed, CostUSD: 0.76, OutputTokens: 900, Timestamp: in},
		{Outcome: stats.OutcomeStalled, Timestamp: in},
		{Outcome: stats.OutcomeCompleted, CostUSD: 1.2, OutputTokens: 500, Timestamp: in},
	}

	for _, n := range reportNotes(records, since, base) {
		if strings.Contains(n, "zero cost and zero tokens") {
			t.Errorf("note %q present, but the only failure is fully accounted and the stall is recorded as a stall", n)
		}
	}
}

// An outcome nothing can currently produce must still be handled: it is not a
// definitive result, so it belongs in no rate — and it is not a stall either,
// so it must not be counted as one. Runs, Stalled and ResolvedRuns are counted
// independently precisely so an unknown value lands in Runs alone.
func TestUnknownOutcomeCountsAsNeitherResolvedNorStalled(t *testing.T) {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{Provider: "codex", Outcome: stats.OutcomeFailed, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeCompleted, Timestamp: in},
		{Provider: "codex", Outcome: stats.OutcomeStalled, Timestamp: in},
		{Provider: "codex", Outcome: "", Timestamp: in},
		{Provider: "codex", Outcome: "some-future-outcome", Timestamp: in},
	}

	got := BreakdownBy(records, since, base, func(r stats.RunRecord) string { return r.Provider })
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	b := got[0]
	if b.Runs != 5 || b.Stalled != 1 || b.ResolvedRuns != 2 || b.Failures != 1 {
		t.Errorf("breakdown = %+v, want runs=5 stalled=1 resolvedRuns=2 failures=1", b)
	}
	if b.FailureRate != 0.5 {
		t.Errorf("FailureRate = %v, want 0.5 (1 of 2 definitive results; the unknown pair must not dilute it)", b.FailureRate)
	}

	sc := Compute(records, nil, since, base)
	if sc.AgentRuns != 5 || sc.AgentStalls != 1 || sc.AgentResolvedRuns != 2 || sc.FailureRate != 0.5 {
		t.Errorf("scorecard = runs:%d stalls:%d resolved:%d rate:%v, want 5/1/2/0.5",
			sc.AgentRuns, sc.AgentStalls, sc.AgentResolvedRuns, sc.FailureRate)
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

func TestBreakdownBySkillExecutionModeNormalization(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{SkillExecutionMode: skillattr.ExecutionModeNative, CostUSD: 1.0, Outcome: "completed", Timestamp: in},
		{SkillExecutionMode: "", CostUSD: 2.0, Outcome: "failed", Timestamp: in},
	}
	got := BreakdownBy(records, since, base, func(r stats.RunRecord) string {
		return skillattr.NormalizeExecutionMode(r.SkillExecutionMode)
	})
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(got), got)
	}
	if got[0].Key != "native" {
		t.Fatalf("first group = %+v, want native", got[0])
	}
	if got[1].Key != "unknown" || got[1].Failures != 1 {
		t.Fatalf("second group = %+v, want unknown with one failure", got[1])
	}
}

func TestCompareByVariant(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "copilot", Model: "gpt-5.5", ExperimentID: "exp", VariantID: "gpt", DurationS: 120, PremiumRequests: 7.5, Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "implementation", Provider: "claude", Model: "opus", ExperimentID: "exp", VariantID: "opus", DurationS: 60, CostUSD: 1.5, Outcome: "failed", Timestamp: in},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
	}
	got := CompareByLatestAuthor(records, events, since, base, 20, func(r stats.RunRecord) string {
		if r.ExperimentID == "" || r.VariantID == "" {
			return ""
		}
		return r.ExperimentID + ":" + r.VariantID
	})
	if len(got) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(got), got)
	}
	gpt := got[0]
	if gpt.Key != "exp:gpt" {
		t.Fatalf("first key = %q, want exp:gpt (sorted): %+v", gpt.Key, got)
	}
	if gpt.Runs != 1 || gpt.Landed != 1 || gpt.MergeRate != 1 || gpt.PremiumRequests != 7.5 || gpt.PremiumRequestsPerLanded != 7.5 {
		t.Fatalf("gpt row = %+v", gpt)
	}
	if gpt.AttributionMode != string(ComparisonAttributionLatestAuthor) {
		t.Fatalf("attribution mode = %q, want latest author", gpt.AttributionMode)
	}
	if !gpt.InsufficientData {
		t.Fatalf("gpt row should be marked low sample: %+v", gpt)
	}
}

func TestCompareByVariantDoesNotAttributePreWindowRun(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "opus", ExperimentID: "exp", VariantID: "opus", Outcome: "completed", Timestamp: since.Add(-1 * time.Hour)},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: base.Add(-1 * time.Hour), Data: map[string]any{"outcome": "merged"}},
	}
	got := CompareByLatestAuthor(records, events, since, base, 0, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID
	})
	if len(got) != 0 {
		t.Fatalf("CompareBy attributed landing to pre-window run: %+v", got)
	}
}

func TestCompareVariantsAggregatesRoles(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", ExperimentID: "exp", VariantID: "a", DurationS: 120, CostUSD: 2, PremiumRequests: 4, Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "fix-review", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", ExperimentID: "exp", VariantID: "a", DurationS: 240, CostUSD: 3, PremiumRequests: 6, Outcome: "failed", Timestamp: in},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in.Add(30 * time.Minute), Data: map[string]any{"outcome": "merged"}},
	}

	got := CompareVariants(records, events, since, base, 0)
	if len(got) != 1 {
		t.Fatalf("variant rows = %d, want 1: %+v", len(got), got)
	}
	parent := got[0]
	if parent.ExperimentID != "exp" || parent.VariantID != "a" || parent.Role != "" {
		t.Fatalf("parent identity = %+v, want exp/a with empty role", parent)
	}
	if parent.Runs != 2 || parent.Failures != 1 || parent.Landed != 1 || parent.MergeRate != 1 || parent.FailureRate != 0.5 {
		t.Fatalf("parent metrics = %+v", parent)
	}
	if parent.TotalCostUSD != 5 || parent.CostPerLanded != 5 || parent.PremiumRequests != 10 || parent.PremiumRequestsPerLanded != 10 {
		t.Fatalf("parent cost/premium metrics = %+v", parent)
	}
	if parent.DurationP50S != 120 || parent.DurationP90S != 240 {
		t.Fatalf("parent duration = p50 %v p90 %v, want 120/240", parent.DurationP50S, parent.DurationP90S)
	}
	if len(parent.RoleBreakdowns) != 2 {
		t.Fatalf("role breakdowns = %d, want 2: %+v", len(parent.RoleBreakdowns), parent.RoleBreakdowns)
	}
	impl := comparisonByRole(t, parent.RoleBreakdowns, "implementation")
	if impl.Runs != 1 || impl.Landed != 1 || impl.MergeRate != 1 || impl.TotalCostUSD != 2 || impl.PremiumRequests != 4 {
		t.Fatalf("implementation child = %+v", impl)
	}
	fix := comparisonByRole(t, parent.RoleBreakdowns, "fix-review")
	if fix.Runs != 1 || fix.Failures != 1 || fix.Landed != 0 || fix.FailureRate != 1 {
		t.Fatalf("fix-review child = %+v", fix)
	}
}

func TestCompareVariantsKeepsChildLowSampleRows(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "testing", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
	}

	got := CompareVariants(records, nil, since, base, 20)
	if len(got) != 1 {
		t.Fatalf("variant rows = %d, want 1: %+v", len(got), got)
	}
	if !got[0].InsufficientData {
		t.Fatalf("parent should be low sample: %+v", got[0])
	}
	if len(got[0].RoleBreakdowns) != 2 {
		t.Fatalf("role breakdowns = %d, want 2: %+v", len(got[0].RoleBreakdowns), got[0].RoleBreakdowns)
	}
	for _, child := range got[0].RoleBreakdowns {
		if !child.InsufficientData {
			t.Fatalf("child should be low sample: %+v", child)
		}
	}
}

func TestCompareVariantsClearsMixedParentMetadata(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{Role: "implementation", Provider: "claude", Model: "sonnet", ReasoningEffort: "medium", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
		{Role: "fix-review", Provider: "codex", Model: "gpt-5", ReasoningEffort: "high", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
	}

	got := CompareVariants(records, nil, since, base, 0)
	if len(got) != 1 {
		t.Fatalf("variant rows = %d, want 1: %+v", len(got), got)
	}
	if got[0].Provider != "" || got[0].Model != "" || got[0].ReasoningEffort != "" {
		t.Fatalf("mixed parent metadata should be cleared: %+v", got[0])
	}
}

func TestCompareVariantsUsesDelimiterSafeIdentity(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", ExperimentID: "exp:a", VariantID: "var:b", Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "fix-review", ExperimentID: "exp:a", VariantID: "var:b", Outcome: "completed", Timestamp: in},
	}

	got := CompareVariants(records, nil, since, base, 0)
	if len(got) != 1 {
		t.Fatalf("variant rows = %d, want 1: %+v", len(got), got)
	}
	if got[0].ExperimentID != "exp:a" || got[0].VariantID != "var:b" {
		t.Fatalf("parent identity lost delimiters: %+v", got[0])
	}
	if len(got[0].RoleBreakdowns) != 2 {
		t.Fatalf("children did not attach to delimiter-containing parent: %+v", got[0])
	}
}

func TestCompareVariantsPreservesLatestAuthorAttribution(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	t1 := base.Add(-3 * time.Hour)
	t2 := base.Add(-2 * time.Hour)
	landedAt := base.Add(-1 * time.Hour)
	revertedAt := base.Add(-30 * time.Minute)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: t1},
		{TaskID: "A", Role: "fix-review", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: t2},
		{TaskID: "B", Role: "implementation", ExperimentID: "exp", VariantID: "a", CostUSD: 4, PremiumRequests: 8, Outcome: "completed", Timestamp: t2},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: landedAt, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "B", Timestamp: landedAt, Data: map[string]any{"outcome": "merged_with_edits"}},
		{Type: audit.EventPRCIFailureDetected, TaskID: "B", Timestamp: t2},
		{Type: audit.EventTaskStatusChanged, TaskID: "B", Timestamp: t2, Data: map[string]any{"from": "in-progress", "to": "in-review"}},
		{Type: audit.EventTaskStatusChanged, TaskID: "B", Timestamp: t2.Add(time.Minute), Data: map[string]any{"from": "in-progress", "to": "in-review"}},
		{Type: audit.EventPRReverted, TaskID: "B", Timestamp: revertedAt},
	}

	got := CompareVariants(records, events, since, base, 0)
	if len(got) != 1 {
		t.Fatalf("variant rows = %d, want 1: %+v", len(got), got)
	}
	parent := got[0]
	if parent.Landed != 2 || parent.Merged != 1 || parent.MergedWithEdits != 1 || parent.CIFirstPassRate != 0.5 || parent.ReworkRate != 0.5 || parent.RevertRate != 0.5 {
		t.Fatalf("parent attribution metrics = %+v", parent)
	}
	impl := comparisonByRole(t, parent.RoleBreakdowns, "implementation")
	if impl.Landed != 1 || impl.MergedWithEdits != 1 || impl.CIFirstPassRate != 0 || impl.ReworkRate != 1 || impl.RevertRate != 1 {
		t.Fatalf("implementation child attribution = %+v", impl)
	}
	fix := comparisonByRole(t, parent.RoleBreakdowns, "fix-review")
	if fix.Landed != 1 || fix.Merged != 1 || fix.CIFirstPassRate != 1 || fix.ReworkRate != 0 || fix.RevertRate != 0 {
		t.Fatalf("fix-review child attribution = %+v", fix)
	}
}

func comparisonByRole(t *testing.T, rows []ComparisonBreakdown, role string) ComparisonBreakdown {
	t.Helper()
	for i := range rows {
		if rows[i].Role == role {
			return rows[i]
		}
	}
	t.Fatalf("role %q not found in %+v", role, rows)
	return ComparisonBreakdown{}
}

func TestCompareByAttributionModes(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	landedAt := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		// Pre-window author must not be credited.
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "old", ExperimentID: "exp", VariantID: "old", Outcome: "completed", Timestamp: since.Add(-1 * time.Nanosecond)},
		// Same key appears twice before landing; contribution mode credits it once.
		{TaskID: "A", Role: "implementation", Provider: "copilot", Model: "gpt", ExperimentID: "exp", VariantID: "gpt", Outcome: "completed", Timestamp: since},
		{TaskID: "A", Role: "implementation", Provider: "copilot", Model: "gpt", ExperimentID: "exp", VariantID: "gpt", Outcome: "completed", Timestamp: landedAt.Add(-10 * time.Minute)},
		// Inclusive landedAt cutoff: this final-stage author is eligible exactly at landing.
		{TaskID: "A", Role: "fix-review", Provider: "claude", Model: "opus", ExperimentID: "exp", VariantID: "opus", Outcome: "completed", Timestamp: landedAt},
		// Reverted but never landed in this window: runs remain visible, quality attribution does not move.
		{TaskID: "B", Role: "implementation", Provider: "codex", Model: "gpt", ExperimentID: "exp", VariantID: "unused", Outcome: "completed", Timestamp: landedAt},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: landedAt, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: landedAt.Add(1 * time.Minute), Data: map[string]any{"outcome": "merged"}}, // duplicate audit event
		{Type: audit.EventPRReverted, TaskID: "A", Timestamp: landedAt.Add(30 * time.Minute)},
		{Type: audit.EventPRReverted, TaskID: "B", Timestamp: landedAt.Add(30 * time.Minute)},
	}
	key := func(r stats.RunRecord) string {
		if r.ExperimentID == "" || r.VariantID == "" {
			return ""
		}
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	}

	latest := CompareByLatestAuthor(records, events, since, base, 0, key)
	latestOpus := mustComparisonRow(t, latest, "exp:opus:fix-review")
	if latestOpus.Landed != 1 || latestOpus.Merged != 1 || latestOpus.RevertRate != 1 {
		t.Fatalf("latest opus row = %+v, want one landed merged revert", latestOpus)
	}
	if latestOpus.AttributionMode != string(ComparisonAttributionLatestAuthor) {
		t.Fatalf("latest attribution mode = %q", latestOpus.AttributionMode)
	}
	if row, ok := comparisonRow(latest, "exp:gpt:implementation"); !ok || row.Landed != 0 || !row.QualityAttributionLimited {
		t.Fatalf("latest gpt row = %+v/%v, want visible limited-attribution run with no landing", row, ok)
	}

	contrib := CompareByContribution(records, events, since, base, 0, key)
	gpt := mustComparisonRow(t, contrib, "exp:gpt:implementation")
	if gpt.Landed != 1 || gpt.Merged != 1 || gpt.RevertRate != 1 {
		t.Fatalf("contribution gpt row = %+v, want one de-duplicated landed merged revert", gpt)
	}
	if gpt.AttributionMode != string(ComparisonAttributionAnyContribution) {
		t.Fatalf("contribution attribution mode = %q", gpt.AttributionMode)
	}
	opus := mustComparisonRow(t, contrib, "exp:opus:fix-review")
	if opus.Landed != 1 || opus.RevertRate != 1 {
		t.Fatalf("contribution opus row = %+v, want same landed cohort revert attribution", opus)
	}
	unused := mustComparisonRow(t, contrib, "exp:unused:implementation")
	if unused.Landed != 0 || unused.RevertRate != 0 || !unused.QualityAttributionLimited {
		t.Fatalf("unused out-of-cohort row = %+v, want no landing/revert quality attribution", unused)
	}
}

func TestWilson95(t *testing.T) {
	got := wilson95(5, 10)
	if !got.HasData {
		t.Fatalf("expected data: %+v", got)
	}
	if got.Numerator != 5 || got.Denominator != 10 || got.Point != 0.5 {
		t.Fatalf("estimate counts/point = %+v", got)
	}
	if math.Abs(got.WilsonLower-0.2366) > 0.0001 || math.Abs(got.WilsonUpper-0.7634) > 0.0001 {
		t.Fatalf("Wilson interval = %.4f..%.4f, want ~0.2366..0.7634", got.WilsonLower, got.WilsonUpper)
	}
	empty := wilson95(0, 0)
	if empty.HasData || empty.Point != 0 || empty.WilsonLower != 0 || empty.WilsonUpper != 0 {
		t.Fatalf("zero denominator estimate = %+v, want no data zeros", empty)
	}
	data, err := json.Marshal([]RateEstimate{got, empty, wilson95(1, -1)})
	if err != nil {
		t.Fatalf("marshal estimates: %v", err)
	}
	if len(data) == 0 || containsNonFinite(got) || containsNonFinite(empty) {
		t.Fatalf("non-finite estimate JSON/data: %s %+v %+v", data, got, empty)
	}
}

func TestCompareByVariantEstimatesAndExperimentStatus(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A1", Role: "implementation", Provider: "claude", Model: "opus", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "A2", Role: "implementation", Provider: "claude", Model: "opus", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceNone, Outcome: "failed", Timestamp: in},
		{TaskID: "B1", Role: "implementation", Provider: "codex", Model: "gpt-5.5", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "B2", Role: "implementation", Provider: "codex", Model: "gpt-5.5", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "C1", Role: "implementation", Provider: "other", Model: "model", ExperimentID: "exp", VariantID: "observed", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A1", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "B1", Timestamp: in, Data: map[string]any{"outcome": "merged_with_edits"}},
		{Type: audit.EventTaskLanded, TaskID: "B2", Timestamp: in, Data: map[string]any{"outcome": "closed"}},
		{Type: audit.EventPRCIFailureDetected, TaskID: "B2", Timestamp: in},
		{Type: audit.EventTaskStatusChanged, TaskID: "B2", Timestamp: in, Data: map[string]any{"from": "testing", "to": "in-review"}},
		{Type: audit.EventTaskStatusChanged, TaskID: "B2", Timestamp: in, Data: map[string]any{"from": "testing", "to": "in-review"}},
		{Type: audit.EventPRReverted, TaskID: "B1", Timestamp: in},
	}
	res := CompareBy(records, events, since, base, CompareOptions{
		MinSamples: 2,
		Experiments: []abtest.Experiment{{
			ID:       "exp",
			Roles:    []string{"implementation"},
			Variants: []abtest.Variant{{ID: "control", Weight: 1}, {ID: "treatment", Weight: 1}, {ID: "missing", Weight: 1}},
		}},
	}, func(r stats.RunRecord) string {
		if r.ExperimentID == "" || r.VariantID == "" {
			return ""
		}
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	})
	rows := rowsByVariant(res.Rows)
	control := rows["control"]
	treatment := rows["treatment"]
	observed := rows["observed"]
	if control == nil || treatment == nil || observed == nil {
		t.Fatalf("rows by variant = %+v", res.Rows)
		return
	}
	if !control.Baseline || control.BaselineVariantID != "control" {
		t.Fatalf("control baseline fields = %+v", *control)
	}
	if !control.FailureEstimate.HasDelta || control.FailureEstimate.DeltaFromBaseline != 0 {
		t.Fatalf("control failure delta = %+v", control.FailureEstimate)
	}
	if treatment.FailureRate != treatment.FailureEstimate.Point || treatment.MergeRate != treatment.MergeEstimate.Point {
		t.Fatalf("scalar rates do not mirror estimates: %+v", *treatment)
	}
	if !treatment.MergeEstimate.HasDelta || treatment.MergeEstimate.DeltaFromBaseline != -1 {
		t.Fatalf("treatment merge delta = %+v, want -1 from baseline", treatment.MergeEstimate)
	}
	if treatment.CIFirstPassEstimate.Point != 0.5 || treatment.MergedWithEditsEstimate.Point != 0.5 ||
		treatment.ReworkEstimate.Point != 0.5 || treatment.RevertEstimate.Point != 0.5 {
		t.Fatalf("treatment secondary estimates = ci %+v edited %+v rework %+v revert %+v",
			treatment.CIFirstPassEstimate, treatment.MergedWithEditsEstimate, treatment.ReworkEstimate, treatment.RevertEstimate)
	}
	if observed.SampleStatus != SampleStatusLowSample || observed.MinSamplesPerVariant != 2 {
		t.Fatalf("observed sample status = %+v", *observed)
	}
	if len(res.Experiments) != 1 {
		t.Fatalf("experiment statuses = %+v", res.Experiments)
	}
	status := res.Experiments[0]
	if status.BaselineVariantID != "control" || status.ReadyVariants != 2 || status.TotalRuns != 5 || status.Status != "directional" {
		t.Fatalf("experiment status = %+v", status)
	}
	if len(status.Variants) != 4 {
		t.Fatalf("variants = %+v, want configured plus observed-unconfigured", status.Variants)
	}
	missing := status.Variants[2]
	if missing.VariantID != "missing" || missing.Observed || !missing.Configured || missing.Ready {
		t.Fatalf("missing configured variant = %+v", missing)
	}
}

func TestCompareByVariantMissingBaselineLeavesDeltasUnset(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "B1", Role: "implementation", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
	}
	res := CompareBy(records, nil, since, base, CompareOptions{
		MinSamples: 1,
		Experiments: []abtest.Experiment{{
			ID:       "exp",
			Roles:    []string{"implementation"},
			Variants: []abtest.Variant{{ID: "control", Weight: 1}, {ID: "treatment", Weight: 1}},
		}},
	}, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	})
	rows := rowsByVariant(res.Rows)
	if rows["treatment"] == nil {
		t.Fatalf("rows = %+v", res.Rows)
		return
	}
	if rows["treatment"].FailureEstimate.HasDelta || rows["treatment"].Baseline {
		t.Fatalf("missing baseline should not set delta/baseline: %+v", *rows["treatment"])
	}
	if len(res.Experiments) != 1 || res.Experiments[0].Status != "directional" {
		t.Fatalf("experiment status = %+v", res.Experiments)
	}
}

func TestCompareByVariantEmptyExperimentRolesUseObservedRoles(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A1", Role: "implementation", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "B1", Role: "implementation", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "failed", Timestamp: in},
	}
	res := CompareBy(records, nil, since, base, CompareOptions{
		MinSamples: 1,
		Experiments: []abtest.Experiment{{
			ID:       "exp",
			Variants: []abtest.Variant{{ID: "control", Weight: 1}, {ID: "treatment", Weight: 1}},
		}},
	}, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	})
	rows := rowsByVariant(res.Rows)
	control := rows["control"]
	treatment := rows["treatment"]
	if control == nil || treatment == nil {
		t.Fatalf("rows = %+v", res.Rows)
		return
	}
	if !control.Baseline || control.BaselineVariantID != "control" {
		t.Fatalf("control baseline fields = %+v", *control)
	}
	if !treatment.FailureEstimate.HasDelta || treatment.FailureEstimate.DeltaFromBaseline != 1 {
		t.Fatalf("treatment failure delta = %+v, want +1 from baseline", treatment.FailureEstimate)
	}
	if len(res.Experiments) != 1 {
		t.Fatalf("experiment statuses = %+v", res.Experiments)
	}
	status := res.Experiments[0]
	if status.ExperimentID != "exp" || status.Role != "implementation" || status.BaselineVariantID != "control" || status.Status != "actionable" {
		t.Fatalf("experiment status = %+v", status)
	}
}

func TestCompareByVariantZeroWeightFirstVariantDoesNotBecomeBaseline(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A1", Role: "implementation", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "B1", Role: "implementation", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "failed", Timestamp: in},
	}
	res := CompareBy(records, nil, since, base, CompareOptions{
		MinSamples: 1,
		Experiments: []abtest.Experiment{{
			ID:    "exp",
			Roles: []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "disabled", Weight: 0},
				{ID: "control", Weight: 1},
				{ID: "treatment", Weight: 1},
			},
		}},
	}, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	})
	rows := rowsByVariant(res.Rows)
	if rows["control"] == nil || !rows["control"].Baseline || rows["control"].BaselineVariantID != "control" {
		t.Fatalf("control baseline fields = %+v", rows["control"])
	}
	if len(res.Experiments) != 1 {
		t.Fatalf("experiment statuses = %+v", res.Experiments)
	}
	status := res.Experiments[0]
	if status.BaselineVariantID != "control" || status.Status != "actionable" || len(status.Variants) != 2 {
		t.Fatalf("experiment status = %+v", status)
	}
	for _, variant := range status.Variants {
		if variant.VariantID == "disabled" {
			t.Fatalf("zero-weight variant included in readiness: %+v", status.Variants)
		}
	}
}

func TestCompareByVariantZeroWeightNonBaselineDoesNotBlockReadiness(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A1", Role: "implementation", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "B1", Role: "implementation", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "C1", Role: "implementation", ExperimentID: "exp", VariantID: "disabled", SkillConformance: skillattr.ConformanceNone, Outcome: "failed", Timestamp: in},
	}
	res := CompareBy(records, nil, since, base, CompareOptions{
		MinSamples: 1,
		Experiments: []abtest.Experiment{{
			ID:    "exp",
			Roles: []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "control", Weight: 1},
				{ID: "disabled", Weight: 0},
				{ID: "treatment", Weight: 1},
			},
		}},
	}, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	})
	if len(res.Experiments) != 1 {
		t.Fatalf("experiment statuses = %+v", res.Experiments)
	}
	status := res.Experiments[0]
	if status.Status != "actionable" || status.ReadyVariants != 2 || status.TotalRuns != 2 || len(status.Variants) != 2 {
		t.Fatalf("experiment status = %+v", status)
	}
	for _, variant := range status.Variants {
		if variant.VariantID == "disabled" {
			t.Fatalf("zero-weight variant included in readiness: %+v", status.Variants)
		}
	}
}

func containsNonFinite(est RateEstimate) bool {
	values := []float64{est.Point, est.WilsonLower, est.WilsonUpper, est.DeltaFromBaseline}
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

func rowsByVariant(rows []ComparisonBreakdown) map[string]*ComparisonBreakdown {
	out := map[string]*ComparisonBreakdown{}
	for i := range rows {
		out[rows[i].VariantID] = &rows[i]
	}
	return out
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

func TestCompute_RevertOfOutOfWindowMergeNotCounted(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -30)
	in := base.Add(-1 * time.Hour)
	events := []audit.Event{
		// One in-window merged landing.
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
		// Reverts in-window, but for tasks that did NOT land in-window → must not
		// count, so the rate can't exceed 100%.
		{Type: audit.EventPRReverted, TaskID: "OLD1", Timestamp: in},
		{Type: audit.EventPRReverted, TaskID: "OLD2", Timestamp: in},
	}
	got := Compute(nil, events, since, base)
	if got.Reverted != 0 {
		t.Errorf("Reverted = %d, want 0 (reverts for out-of-window landings excluded)", got.Reverted)
	}
	if got.ChangeFailureRate != 0 {
		t.Errorf("ChangeFailureRate = %v, want 0 (no in-cohort reverts)", got.ChangeFailureRate)
	}
}

type staticStats struct {
	records []stats.RunRecord
}

func (s staticStats) All() []stats.RunRecord { return append([]stats.RunRecord(nil), s.records...) }

func TestServiceScanPopulatesAttributionReports(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	landedAt := now.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "copilot", Model: "gpt", ReasoningEffort: "high", ExperimentID: "exp", VariantID: "gpt", Outcome: "completed", Timestamp: landedAt.Add(-30 * time.Minute)},
		{TaskID: "A", Role: "pr-fix", Provider: "claude", Model: "opus", ReasoningEffort: "medium", ExperimentID: "exp", VariantID: "opus", Outcome: "completed", Timestamp: landedAt},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: landedAt, Data: map[string]any{"outcome": "merged"}},
	}
	svc := NewService(Deps{
		Cfg: config.EvaluationConfig{WindowDays: 7},
		ABTesting: abtest.Config{Experiments: []abtest.Experiment{
			{ID: "exp", Variants: []abtest.Variant{{ID: "gpt"}, {ID: "opus"}}},
		}},
		Stats: staticStats{records: records},
		Audit: auditFunc(func(audit.Query) ([]audit.Event, error) { return events, nil }),
		Now:   func() time.Time { return now },
	})
	rep, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	modelKind := mustExperimentKind(t, rep.ByExperimentKind, "model")
	modelGroup := mustExperimentGroup(t, modelKind.Groups, "exp")
	if len(rep.ByAgentModel) == 0 || len(rep.ByAgentModelContribution) == 0 || len(modelGroup.Rows) == 0 || len(modelGroup.RowsContribution) == 0 {
		t.Fatalf("missing attribution report slices: agent=%d agentContrib=%d variant=%d variantContrib=%d", len(rep.ByAgentModel), len(rep.ByAgentModelContribution), len(modelGroup.Rows), len(modelGroup.RowsContribution))
	}
	if row := mustComparisonVariant(t, modelGroup.Rows, "opus"); row.Landed != 1 || row.AttributionMode != string(ComparisonAttributionLatestAuthor) {
		t.Fatalf("latest variant row = %+v, want final-stage opus landing", row)
	}
	if row := mustComparisonVariant(t, modelGroup.RowsContribution, "gpt"); row.Landed != 1 || row.AttributionMode != string(ComparisonAttributionAnyContribution) {
		t.Fatalf("contribution variant row = %+v, want gpt contribution landing", row)
	} else if child := comparisonByRole(t, row.RoleBreakdowns, "implementation"); child.Landed != 1 {
		t.Fatalf("contribution variant child = %+v, want implementation gpt contribution landing", child)
	}
}

func mustExperimentKind(t *testing.T, groups []ExperimentKindBreakdown, kind string) ExperimentKindBreakdown {
	t.Helper()
	for i := range groups {
		if groups[i].Kind == kind {
			return groups[i]
		}
	}
	t.Fatalf("missing experiment kind %q in %+v", kind, groups)
	return ExperimentKindBreakdown{}
}

func mustExperimentGroup(t *testing.T, groups []ExperimentGroup, experimentID string) ExperimentGroup {
	t.Helper()
	for i := range groups {
		if groups[i].ExperimentID == experimentID {
			return groups[i]
		}
	}
	t.Fatalf("missing experiment group %q in %+v", experimentID, groups)
	return ExperimentGroup{}
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

func mustComparisonRow(t *testing.T, rows []ComparisonBreakdown, key string) ComparisonBreakdown {
	t.Helper()
	row, ok := comparisonRow(rows, key)
	if !ok {
		t.Fatalf("missing comparison row %q in %+v", key, rows)
	}
	return row
}

func mustComparisonVariant(t *testing.T, rows []ComparisonBreakdown, variant string) ComparisonBreakdown {
	t.Helper()
	for i := range rows {
		if rows[i].VariantID == variant {
			return rows[i]
		}
	}
	t.Fatalf("missing comparison variant %q in %+v", variant, rows)
	return ComparisonBreakdown{}
}

func comparisonRow(rows []ComparisonBreakdown, key string) (ComparisonBreakdown, bool) {
	for i := range rows {
		if rows[i].Key == key {
			return rows[i], true
		}
	}
	return ComparisonBreakdown{}, false
}

func TestGroupByKindSeparatesModelAndPromptExperiments(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", ExperimentID: "model-exp", VariantID: "a", Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "implementation", Provider: "claude", Model: "sonnet", ExperimentID: "prompt-exp", VariantID: "p1", Outcome: "completed", Timestamp: in},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "B", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
	}
	exps := []abtest.Experiment{
		{ID: "model-exp", Roles: []string{"implementation"}, Variants: []abtest.Variant{{ID: "a", Weight: 1}}},
		{ID: "prompt-exp", Kind: "prompt", Roles: []string{"implementation"}, Variants: []abtest.Variant{{ID: "p1", Provider: "claude", Model: "sonnet", Weight: 1}}},
	}
	opts := CompareOptions{Experiments: exps}
	latest := compareVariantsByAttribution(records, events, since, base, opts, ComparisonAttributionLatestAuthor)
	contrib := compareVariantsByAttribution(records, events, since, base, opts, ComparisonAttributionAnyContribution)

	groups := GroupByKind(latest, contrib, exps)
	model := mustExperimentKind(t, groups, "model")
	prompt := mustExperimentKind(t, groups, "prompt")
	modelGroup := mustExperimentGroup(t, model.Groups, "model-exp")
	promptGroup := mustExperimentGroup(t, prompt.Groups, "prompt-exp")
	if len(modelGroup.Rows) != 1 || modelGroup.Rows[0].ExperimentID != "model-exp" || modelGroup.Rows[0].Kind != "model" {
		t.Fatalf("model group = %+v", modelGroup)
	}
	if len(promptGroup.Rows) != 1 || promptGroup.Rows[0].ExperimentID != "prompt-exp" || promptGroup.Rows[0].Kind != "prompt" {
		t.Fatalf("prompt group = %+v", promptGroup)
	}
}

func TestGroupByKindSeparatesDifferentSubjectsInSameKind(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", ExperimentID: "prompt-author", VariantID: "p1", Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "review", Provider: "codex", Model: "gpt-5.5", ExperimentID: "prompt-review", VariantID: "r1", Outcome: "completed", Timestamp: in},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
		{Type: audit.EventTaskLanded, TaskID: "B", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
	}
	exps := []abtest.Experiment{
		{
			ID:       "prompt-author",
			Kind:     "prompt",
			Subject:  &abtest.Subject{WorkflowID: "wf-a", StepID: "author", Role: "implementation"},
			Roles:    []string{"implementation"},
			Variants: []abtest.Variant{{ID: "p1", Provider: "claude", Model: "sonnet", Weight: 1}},
		},
		{
			ID:       "prompt-review",
			Kind:     "prompt",
			Subject:  &abtest.Subject{WorkflowID: "wf-b", StepID: "review", Role: "review"},
			Roles:    []string{"review"},
			Variants: []abtest.Variant{{ID: "r1", Provider: "codex", Model: "gpt-5.5", Weight: 1}},
		},
	}
	opts := CompareOptions{Experiments: exps}
	latest := compareVariantsByAttribution(records, events, since, base, opts, ComparisonAttributionLatestAuthor)
	contrib := compareVariantsByAttribution(records, events, since, base, opts, ComparisonAttributionAnyContribution)

	groups := GroupByKind(latest, contrib, exps)
	prompt := mustExperimentKind(t, groups, "prompt")
	if len(prompt.Groups) != 2 {
		t.Fatalf("prompt groups = %+v, want 2 separate experiment groups", prompt.Groups)
	}
	author := mustExperimentGroup(t, prompt.Groups, "prompt-author")
	review := mustExperimentGroup(t, prompt.Groups, "prompt-review")
	if len(author.Rows) != 1 || author.Rows[0].VariantID != "p1" {
		t.Fatalf("author group = %+v, want only the prompt-author row", author)
	}
	if len(review.Rows) != 1 || review.Rows[0].VariantID != "r1" {
		t.Fatalf("review group = %+v, want only the prompt-review row", review)
	}
	if author.Subject == nil || author.Subject.StepID != "author" {
		t.Fatalf("author group subject = %+v", author.Subject)
	}
	if review.Subject == nil || review.Subject.StepID != "review" {
		t.Fatalf("review group subject = %+v", review.Subject)
	}
}

func TestGroupByKindUnresolvedExperimentIsUnknown(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", ExperimentID: "retired-exp", VariantID: "x", Outcome: "completed", Timestamp: in},
	}
	opts := CompareOptions{}
	latest := compareVariantsByAttribution(records, nil, since, base, opts, ComparisonAttributionLatestAuthor)
	contrib := compareVariantsByAttribution(records, nil, since, base, opts, ComparisonAttributionAnyContribution)

	groups := GroupByKind(latest, contrib, nil)
	unknown := mustExperimentKind(t, groups, "unknown")
	unknownGroup := mustExperimentGroup(t, unknown.Groups, "retired-exp")
	if len(unknownGroup.Rows) != 1 || unknownGroup.Rows[0].ExperimentID != "retired-exp" || unknownGroup.Rows[0].Kind != "unknown" {
		t.Fatalf("unknown group = %+v", unknownGroup)
	}
}

func TestGroupByKindPromptSkillRowsExposeFixedProviderModel(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", ExperimentID: "skill-exp", VariantID: "s1", Outcome: "completed", Timestamp: in},
	}
	exps := []abtest.Experiment{
		{
			ID:       "skill-exp",
			Kind:     "skill",
			Subject:  &abtest.Subject{SkillName: "sybra-tasks"},
			Roles:    []string{"implementation"},
			Variants: []abtest.Variant{{ID: "s1", Provider: "claude", Model: "sonnet", Weight: 1}},
		},
	}
	opts := CompareOptions{Experiments: exps}
	latest := compareVariantsByAttribution(records, nil, since, base, opts, ComparisonAttributionLatestAuthor)
	contrib := compareVariantsByAttribution(records, nil, since, base, opts, ComparisonAttributionAnyContribution)

	groups := GroupByKind(latest, contrib, exps)
	skill := mustExperimentKind(t, groups, "skill")
	skillGroup := mustExperimentGroup(t, skill.Groups, "skill-exp")
	if len(skillGroup.Rows) != 1 {
		t.Fatalf("skill rows = %+v", skillGroup.Rows)
	}
	row := skillGroup.Rows[0]
	if row.Provider != "claude" || row.Model != "sonnet" {
		t.Fatalf("skill row provider/model = %+v, want fixed claude/sonnet visible", row)
	}
	if row.Subject == nil || row.Subject.SkillName != "sybra-tasks" {
		t.Fatalf("skill row subject = %+v", row.Subject)
	}
}

func TestGroupByKindLowSampleRowsFlagged(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", ExperimentID: "prompt-exp", VariantID: "p1", Outcome: "completed", Timestamp: in},
	}
	exps := []abtest.Experiment{
		{ID: "prompt-exp", Kind: "prompt", Roles: []string{"implementation"}, Variants: []abtest.Variant{{ID: "p1", Weight: 1}}},
	}
	opts := CompareOptions{MinSamples: 20, Experiments: exps}
	latest := compareVariantsByAttribution(records, nil, since, base, opts, ComparisonAttributionLatestAuthor)
	contrib := compareVariantsByAttribution(records, nil, since, base, opts, ComparisonAttributionAnyContribution)

	groups := GroupByKind(latest, contrib, exps)
	prompt := mustExperimentKind(t, groups, "prompt")
	promptGroup := mustExperimentGroup(t, prompt.Groups, "prompt-exp")
	if len(promptGroup.Rows) != 1 || !promptGroup.Rows[0].InsufficientData {
		t.Fatalf("prompt rows = %+v, want a visible low-sample row", promptGroup.Rows)
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

func TestSkillConformanceBucket(t *testing.T) {
	tests := []struct {
		conformance string
		want        string
	}{
		{skillattr.ConformanceExact, SkillCohortSkill},
		{skillattr.ConformanceRecovered, SkillCohortSkill},
		{skillattr.ConformanceFallback, SkillCohortDirect},
		{skillattr.ConformanceUnavailable, SkillCohortDirect},
		{skillattr.ConformanceNone, SkillCohortDirect},
		{skillattr.ConformanceUnverified, SkillCohortIndeterminate},
		{skillattr.ConformanceUnknown, SkillCohortIndeterminate},
		{"", SkillCohortIndeterminate},
		{"garbage", SkillCohortIndeterminate},
	}
	for _, tt := range tests {
		t.Run(tt.conformance, func(t *testing.T) {
			if got := skillConformanceBucket(tt.conformance); got != tt.want {
				t.Errorf("skillConformanceBucket(%q) = %q, want %q", tt.conformance, got, tt.want)
			}
		})
	}
}

// TestAgentModelCohortKeySplitsDirectFromConformantReview covers the issue's
// own example: "A direct review cannot appear in the same cohort as a
// conformant staff review."
func TestAgentModelCohortKeySplitsDirectFromConformantReview(t *testing.T) {
	conformant := stats.RunRecord{Provider: "claude", Model: "sonnet", Role: "review", SkillConformance: skillattr.ConformanceExact}
	direct := stats.RunRecord{Provider: "claude", Model: "sonnet", Role: "review", SkillConformance: skillattr.ConformanceNone}
	kc := agentModelCohortKey(conformant)
	kd := agentModelCohortKey(direct)
	if kc == kd {
		t.Fatalf("conformant and direct review runs shared a cohort key %q", kc)
	}
	if !strings.HasSuffix(kc, ":"+SkillCohortSkill) {
		t.Fatalf("conformant key = %q, want suffix :%s", kc, SkillCohortSkill)
	}
	if !strings.HasSuffix(kd, ":"+SkillCohortDirect) {
		t.Fatalf("direct key = %q, want suffix :%s", kd, SkillCohortDirect)
	}
}

func TestCompareByAgentModelSplitsDirectFromConformantReview(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "review", Provider: "claude", Model: "sonnet", SkillConformance: skillattr.ConformanceExact, Outcome: "completed", Timestamp: in},
		{TaskID: "B", Role: "review", Provider: "claude", Model: "sonnet", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "C", Role: "review", Provider: "claude", Model: "sonnet", SkillConformance: skillattr.ConformanceNone, Outcome: "failed", Timestamp: in},
	}
	got := CompareByLatestAuthor(records, nil, since, base, 0, agentModelCohortKey)
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (skill, direct): %+v", len(got), got)
	}
	var skillRow, directRow *ComparisonBreakdown
	for i := range got {
		switch got[i].SkillConformance {
		case SkillCohortSkill:
			skillRow = &got[i]
		case SkillCohortDirect:
			directRow = &got[i]
		}
	}
	if skillRow == nil || directRow == nil {
		t.Fatalf("rows = %+v, want one skill row and one direct row", got)
	}
	if skillRow.Runs != 1 {
		t.Fatalf("skill row = %+v, want 1 run", *skillRow)
	}
	if directRow.Runs != 2 || directRow.Failures != 1 {
		t.Fatalf("direct row = %+v, want 2 runs / 1 failure", *directRow)
	}
}

func TestComparisonRowsHomogeneousDirectConformanceNotInsufficient(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", ExperimentID: "exp", VariantID: "v", SkillConformance: skillattr.ConformanceFallback, Outcome: "completed", Timestamp: in},
		{TaskID: "B", ExperimentID: "exp", VariantID: "v", SkillConformance: skillattr.ConformanceUnavailable, Outcome: "completed", Timestamp: in},
	}
	got := CompareByLatestAuthor(records, nil, since, base, 2, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID
	})
	if len(got) != 1 {
		t.Fatalf("rows = %+v, want 1", got)
	}
	row := got[0]
	if row.SkillConformance != SkillCohortDirect {
		t.Fatalf("row skill conformance = %q, want %q (fallback + unavailable both bucket to direct, not dropped)", row.SkillConformance, SkillCohortDirect)
	}
	if row.SkillParityUnknown {
		t.Fatalf("row = %+v, homogeneous direct cohort should not be parity-unknown", row)
	}
	if row.InsufficientData {
		t.Fatalf("row = %+v, sample size meets minSamples so should not be insufficient", row)
	}
}

func TestComparisonRowsIndeterminateSkillConformanceForcesInsufficient(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	// Legacy/pre-instrumentation records: SkillConformance never set.
	records := make([]stats.RunRecord, 0, 20)
	for range 20 {
		records = append(records, stats.RunRecord{TaskID: "A", ExperimentID: "exp", VariantID: "v", Outcome: "completed", Timestamp: in})
	}
	got := CompareByLatestAuthor(records, nil, since, base, 2, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID
	})
	if len(got) != 1 {
		t.Fatalf("rows = %+v, want 1", got)
	}
	row := got[0]
	if row.Runs != 20 {
		t.Fatalf("row runs = %d, want 20 (data is not dropped, just marked untrustworthy)", row.Runs)
	}
	if row.SkillConformance != SkillCohortIndeterminate {
		t.Fatalf("row skill conformance = %q, want %q", row.SkillConformance, SkillCohortIndeterminate)
	}
	if !row.SkillParityUnknown {
		t.Fatalf("row = %+v, want SkillParityUnknown", row)
	}
	if !row.InsufficientData {
		t.Fatalf("row = %+v, want InsufficientData despite a 20-run sample — history with unknown skill parity is never sufficient", row)
	}
	if row.SampleStatus != SampleStatusParityUnknown {
		t.Fatalf("row sample status = %q, want %q", row.SampleStatus, SampleStatusParityUnknown)
	}
}

func TestComparisonRowsMixedSkillConformanceForcesInsufficient(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", ExperimentID: "exp", VariantID: "v", SkillConformance: skillattr.ConformanceExact, Outcome: "completed", Timestamp: in},
		{TaskID: "B", ExperimentID: "exp", VariantID: "v", SkillConformance: skillattr.ConformanceFallback, Outcome: "completed", Timestamp: in},
	}
	// variantKey does not fold the skill bucket into the key (unlike
	// agentModelCohortKey), so a variant that actually delivered both skill
	// and direct-fallback executions still lands in one row here.
	got := CompareByLatestAuthor(records, nil, since, base, 2, variantKey)
	if len(got) != 1 {
		t.Fatalf("rows = %+v, want 1", got)
	}
	row := got[0]
	if row.SkillConformance != "" {
		t.Fatalf("row skill conformance = %q, want \"\" (mixed)", row.SkillConformance)
	}
	if !row.SkillParityUnknown || !row.InsufficientData {
		t.Fatalf("row = %+v, a mixed-delivery cohort must never read as a sufficient parity comparison", row)
	}
	if row.SampleStatus != SampleStatusParityUnknown {
		t.Fatalf("row sample status = %q, want %q", row.SampleStatus, SampleStatusParityUnknown)
	}
}

func TestExperimentSampleStatusBlocksReadinessOnSkillParityUnknown(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A1", Role: "implementation", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceExact, Outcome: "completed", Timestamp: in},
		{TaskID: "A2", Role: "implementation", ExperimentID: "exp", VariantID: "control", SkillConformance: skillattr.ConformanceFallback, Outcome: "completed", Timestamp: in},
		{TaskID: "B1", Role: "implementation", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		{TaskID: "B2", Role: "implementation", ExperimentID: "exp", VariantID: "treatment", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
	}
	res := CompareBy(records, nil, since, base, CompareOptions{
		MinSamples: 2,
		Experiments: []abtest.Experiment{{
			ID:       "exp",
			Roles:    []string{"implementation"},
			Variants: []abtest.Variant{{ID: "control", Weight: 1}, {ID: "treatment", Weight: 1}},
		}},
	}, func(r stats.RunRecord) string {
		return r.ExperimentID + ":" + r.VariantID + ":" + normalizedRole(r.Role)
	})
	rows := rowsByVariant(res.Rows)
	if rows["control"] == nil || !rows["control"].SkillParityUnknown {
		t.Fatalf("control row = %+v, want SkillParityUnknown (mixes skill + direct)", rows["control"])
	}
	if rows["treatment"] == nil || rows["treatment"].SkillParityUnknown {
		t.Fatalf("treatment row = %+v, want a clean direct cohort (uniform none)", rows["treatment"])
	}
	if len(res.Experiments) != 1 {
		t.Fatalf("experiment statuses = %+v", res.Experiments)
	}
	status := res.Experiments[0]
	// treatment meets MinSamples with a clean cohort, control has the same
	// sample size but unknown parity — the experiment is directional, not
	// fully actionable, and the routing consumer (internal/learning) must
	// not treat it as a clean win either way.
	if status.Status != "directional" || status.ReadyVariants != 1 {
		t.Fatalf("experiment status = %+v, want directional with exactly 1 ready variant", status)
	}
	for _, v := range status.Variants {
		if v.VariantID == "control" && v.Ready {
			t.Fatalf("control variant = %+v, must not be ready with unknown skill parity", v)
		}
		if v.VariantID == "treatment" && !v.Ready {
			t.Fatalf("treatment variant = %+v, want ready", v)
		}
	}
	byVariant := map[string]VariantSampleStatus{}
	for _, v := range status.Variants {
		byVariant[v.VariantID] = v
	}
	if byVariant["control"].SampleStatus != SampleStatusParityUnknown {
		t.Fatalf("control variant sample status = %q, want %q", byVariant["control"].SampleStatus, SampleStatusParityUnknown)
	}
	if byVariant["treatment"].SampleStatus != SampleStatusActionable {
		t.Fatalf("treatment variant sample status = %q, want %q", byVariant["treatment"].SampleStatus, SampleStatusActionable)
	}
}

func TestReportNotesFlagsIndeterminateSkillConformance(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{Outcome: "completed", Timestamp: in},                                               // indeterminate: unset
		{Outcome: "completed", SkillConformance: skillattr.ConformanceExact, Timestamp: in}, // known
	}
	notes := reportNotes(records, since, base)
	found := false
	for _, n := range notes {
		if strings.Contains(n, "indeterminate skill conformance") {
			found = true
			if !strings.Contains(n, "1 of 2") {
				t.Fatalf("note = %q, want counts 1 of 2", n)
			}
		}
	}
	if !found {
		t.Fatalf("notes = %v, want a skill-parity note", notes)
	}
}

func TestReportNotesOmitsSkillParityNoteWhenAllConformanceKnown(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	since := base.AddDate(0, 0, -7)
	in := base.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{Outcome: "completed", SkillConformance: skillattr.ConformanceExact, Timestamp: in},
		{Outcome: "completed", SkillConformance: skillattr.ConformanceNone, Timestamp: in},
	}
	notes := reportNotes(records, since, base)
	for _, n := range notes {
		if strings.Contains(n, "indeterminate skill conformance") {
			t.Fatalf("notes = %v, unexpected skill-parity note when conformance is fully known", notes)
		}
	}
}
