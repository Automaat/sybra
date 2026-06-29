package evaluation

import (
	"context"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
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
		Cfg:   config.EvaluationConfig{WindowDays: 7},
		Stats: staticStats{records: records},
		Audit: auditFunc(func(audit.Query) ([]audit.Event, error) { return events, nil }),
		Now:   func() time.Time { return now },
	})
	rep, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(rep.ByAgentModel) == 0 || len(rep.ByAgentModelContribution) == 0 || len(rep.ByVariant) == 0 || len(rep.ByVariantContribution) == 0 {
		t.Fatalf("missing attribution report slices: agent=%d agentContrib=%d variant=%d variantContrib=%d", len(rep.ByAgentModel), len(rep.ByAgentModelContribution), len(rep.ByVariant), len(rep.ByVariantContribution))
	}
	if row := mustComparisonVariant(t, rep.ByVariant, "opus"); row.Landed != 1 || row.AttributionMode != string(ComparisonAttributionLatestAuthor) {
		t.Fatalf("latest variant row = %+v, want final-stage opus landing", row)
	}
	if row := mustComparisonVariant(t, rep.ByVariantContribution, "gpt"); row.Landed != 1 || row.AttributionMode != string(ComparisonAttributionAnyContribution) {
		t.Fatalf("contribution variant row = %+v, want gpt contribution landing", row)
	} else if child := comparisonByRole(t, row.RoleBreakdowns, "implementation"); child.Landed != 1 {
		t.Fatalf("contribution variant child = %+v, want implementation gpt contribution landing", child)
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
