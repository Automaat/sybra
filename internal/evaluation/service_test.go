package evaluation

import (
	"context"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/stats"
)

type testStatsReader struct {
	records []stats.RunRecord
}

func (r testStatsReader) All() []stats.RunRecord {
	return r.records
}

func TestServiceScanBuildsVariantParentsWithRoleBreakdowns(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	svc := NewService(Deps{
		Cfg: config.EvaluationConfig{WindowDays: 7},
		ABTesting: abtest.Config{Experiments: []abtest.Experiment{
			{ID: "exp", Variants: []abtest.Variant{{ID: "a"}}},
		}},
		Stats: testStatsReader{records: []stats.RunRecord{
			{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
			{TaskID: "B", Role: "fix-review", Provider: "claude", Model: "sonnet", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
		}},
		Audit: auditFunc(func(q audit.Query) ([]audit.Event, error) {
			return []audit.Event{
				{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
				{Type: audit.EventTaskLanded, TaskID: "B", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
			}, nil
		}),
		Now: func() time.Time { return now },
	})

	got, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	modelKind := mustExperimentKind(t, got.ByExperimentKind, "model")
	modelGroup := mustExperimentGroup(t, modelKind.Groups, "exp")
	if len(modelGroup.Rows) != 1 {
		t.Fatalf("model kind rows = %d, want 1 aggregate parent: %+v", len(modelGroup.Rows), modelGroup.Rows)
	}
	parent := modelGroup.Rows[0]
	if parent.ExperimentID != "exp" || parent.VariantID != "a" || parent.Role != "" || parent.Runs != 2 || parent.Landed != 2 {
		t.Fatalf("model kind parent = %+v, want aggregate exp/a row across roles", parent)
	}
	if len(parent.RoleBreakdowns) != 2 {
		t.Fatalf("roleBreakdowns = %d, want 2: %+v", len(parent.RoleBreakdowns), parent.RoleBreakdowns)
	}
	if got.ByAgentModel[0].RoleBreakdowns != nil {
		t.Fatalf("ByAgentModel should remain flat, got nested rows: %+v", got.ByAgentModel[0])
		panic("unreachable")
	}
}

func TestServiceScanHandlesRecordsWithoutExperimentMetadata(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	svc := NewService(Deps{
		Cfg: config.EvaluationConfig{WindowDays: 7},
		Stats: testStatsReader{records: []stats.RunRecord{
			{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", Outcome: "completed", Timestamp: in},
		}},
		Audit: auditFunc(func(q audit.Query) ([]audit.Event, error) {
			return []audit.Event{
				{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
			}, nil
		}),
		Now: func() time.Time { return now },
	})

	got, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	if len(got.ByExperimentKind) != 0 {
		t.Fatalf("ByExperimentKind = %+v, want empty for records with no experiment metadata", got.ByExperimentKind)
	}
	if len(got.ByAgentModel) == 0 {
		t.Fatalf("ByAgentModel should still populate from non-experiment records")
	}
}

// TestServiceScanPopulatesByCostTierAndBaseline verifies Scan wires the
// cost-tier segmentation and the prior-window baseline: ByCostTier groups by
// provider:role:tier, and CostPerMergedBaseline is populated only once the
// prior equal-length window has landed enough merges to trust
// (minMergedForSignal), nil otherwise.
func TestServiceScanPopulatesByCostTierAndBaseline(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	prior := now.AddDate(0, 0, -10) // inside the prior 7-day window (since=-7d, prior=-14d..-7d)

	var records []stats.RunRecord
	var events []audit.Event
	records = append(records, stats.RunRecord{TaskID: "A", Role: "implementation", Provider: providerid.Claude, Model: "sonnet", CostUSD: 4.0, Outcome: "completed", Timestamp: in})
	events = append(events, audit.Event{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}})
	for i := range minMergedForSignal {
		tid := "prior-" + string(rune('A'+i))
		records = append(records, stats.RunRecord{TaskID: tid, Role: "implementation", Provider: providerid.Claude, Model: "sonnet", CostUSD: 1.0, Outcome: "completed", Timestamp: prior})
		events = append(events, audit.Event{Type: audit.EventTaskLanded, TaskID: tid, Timestamp: prior, Data: map[string]any{"outcome": "merged"}})
	}

	svc := NewService(Deps{
		Cfg:   config.EvaluationConfig{WindowDays: 7},
		Stats: testStatsReader{records: records},
		Audit: auditFunc(func(q audit.Query) ([]audit.Event, error) { return events, nil }),
		Now:   func() time.Time { return now },
	})

	got, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	if len(got.ByCostTier) != 1 || got.ByCostTier[0].Key != "claude:implementation:cheap" {
		t.Fatalf("ByCostTier = %+v, want one claude:implementation:cheap row", got.ByCostTier)
	}
	if got.CostPerMergedBaseline == nil {
		t.Fatal("CostPerMergedBaseline = nil, want populated (prior window has minMergedForSignal merges)")
		panic("unreachable")
	}
	if got.CostPerMergedBaseline.MergedPRs != minMergedForSignal || got.CostPerMergedBaseline.CostPerMergedUSD != 1.0 {
		t.Fatalf("CostPerMergedBaseline = %+v, want mergedPRs=%d costPerMerged=1.0", got.CostPerMergedBaseline, minMergedForSignal)
	}
}

// TestServiceScanBaselineNilWhenPriorWindowThin verifies the baseline is
// omitted (not zero-valued) when the prior window didn't land enough merges
// to trust, so Weaknesses can't mistake "no signal" for "cost dropped to
// zero."
func TestServiceScanBaselineNilWhenPriorWindowThin(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	svc := NewService(Deps{
		Cfg:   config.EvaluationConfig{WindowDays: 7},
		Stats: testStatsReader{records: []stats.RunRecord{{TaskID: "A", Role: "implementation", Provider: providerid.Claude, Model: "sonnet", CostUSD: 4.0, Outcome: "completed", Timestamp: in}}},
		Audit: auditFunc(func(q audit.Query) ([]audit.Event, error) {
			return []audit.Event{{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}}}, nil
		}),
		Now: func() time.Time { return now },
	})

	got, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	if got.CostPerMergedBaseline != nil {
		t.Fatalf("CostPerMergedBaseline = %+v, want nil (no prior-window data)", got.CostPerMergedBaseline)
		panic("unreachable")
	}
}

func findExperimentKind(groups []ExperimentKindBreakdown, kind string) *ExperimentKindBreakdown {
	for i := range groups {
		if groups[i].Kind == kind {
			return &groups[i]
		}
	}
	return nil
}

// TestService_SetABTesting_UpdatesNextScan verifies the routing.WeightApplier
// push path: a config swapped in after construction — not just the Deps.
// ABTesting passed to NewService — must be what the next Scan groups by, so
// a routing overlay tick reaches the Evaluation report without a restart.
func TestService_SetABTesting_UpdatesNextScan(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	records := []stats.RunRecord{
		{TaskID: "A", Role: "implementation", Provider: "claude", Model: "sonnet", ExperimentID: "exp", VariantID: "a", Outcome: "completed", Timestamp: in},
	}
	events := []audit.Event{
		{Type: audit.EventTaskLanded, TaskID: "A", Timestamp: in, Data: map[string]any{"outcome": "merged"}},
	}
	svc := NewService(Deps{
		Cfg:       config.EvaluationConfig{WindowDays: 7},
		ABTesting: abtest.Config{}, // no experiments configured at construction time
		Stats:     testStatsReader{records: records},
		Audit:     auditFunc(func(q audit.Query) ([]audit.Event, error) { return events, nil }),
		Now:       func() time.Time { return now },
	})

	before, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	// Run records still carry the exp/a attribution, but with no matching
	// abtest.Config experiment it classifies as "unknown", not "model".
	if kind := findExperimentKind(before.ByExperimentKind, "model"); kind != nil {
		t.Fatalf("ByExperimentKind[model] before SetABTesting = %+v, want none (no experiment configured)", kind)
		panic("unreachable")
	}
	if kind := findExperimentKind(before.ByExperimentKind, "unknown"); kind == nil {
		t.Fatalf("ByExperimentKind before SetABTesting = %+v, want an unknown-kind group", before.ByExperimentKind)
		panic("unreachable")
	}

	svc.SetABTesting(abtest.Config{Experiments: []abtest.Experiment{
		{ID: "exp", Variants: []abtest.Variant{{ID: "a"}}},
	}})

	after, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	modelKind := mustExperimentKind(t, after.ByExperimentKind, "model")
	group := mustExperimentGroup(t, modelKind.Groups, "exp")
	if len(group.Rows) != 1 || group.Rows[0].VariantID != "a" {
		t.Fatalf("group.Rows after SetABTesting = %+v, want one exp/a row", group.Rows)
	}
}

func TestServiceScanGroupsSkillExecutionMode(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	svc := NewService(Deps{
		Cfg: config.EvaluationConfig{WindowDays: 7},
		Stats: testStatsReader{records: []stats.RunRecord{
			{TaskID: "A", SkillExecutionMode: skillattr.ExecutionModeNone, Outcome: "completed", Timestamp: in},
			{TaskID: "B", SkillExecutionMode: skillattr.ExecutionModeNative, Outcome: "failed", Timestamp: in},
			{TaskID: "C", SkillExecutionMode: "", Outcome: "completed", Timestamp: in},
		}},
		Audit: auditFunc(func(q audit.Query) ([]audit.Event, error) { return nil, nil }),
		Now:   func() time.Time { return now },
	})

	got, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	if len(got.BySkillExecutionMode) != 3 {
		t.Fatalf("BySkillExecutionMode len = %d, want 3: %+v", len(got.BySkillExecutionMode), got.BySkillExecutionMode)
	}
	if got.BySkillExecutionMode[0].Key != "native" || got.BySkillExecutionMode[0].Failures != 1 {
		t.Fatalf("native breakdown = %+v", got.BySkillExecutionMode[0])
	}
	if got.BySkillExecutionMode[1].Key != "none" {
		t.Fatalf("none breakdown = %+v", got.BySkillExecutionMode[1])
	}
	if got.BySkillExecutionMode[2].Key != "unknown" {
		t.Fatalf("unknown breakdown = %+v", got.BySkillExecutionMode[2])
	}
}

// TestServiceScanSplitsDirectReviewFromConformantStaffReview is the
// end-to-end workflow case for #2007: a direct review and a conformant staff
// review sharing every other dimension (provider, model, role) must land in
// separate ByAgentModel rows, each still visible with its own sample size.
func TestServiceScanSplitsDirectReviewFromConformantStaffReview(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	in := now.Add(-1 * time.Hour)
	svc := NewService(Deps{
		Cfg: config.EvaluationConfig{WindowDays: 7},
		Stats: testStatsReader{records: []stats.RunRecord{
			{TaskID: "A", Role: "review", Provider: "claude", Model: "sonnet", SkillConformance: skillattr.ConformanceExact, Outcome: "completed", Timestamp: in},
			{TaskID: "B", Role: "review", Provider: "claude", Model: "sonnet", SkillConformance: skillattr.ConformanceNone, Outcome: "completed", Timestamp: in},
		}},
		Audit: auditFunc(func(q audit.Query) ([]audit.Event, error) { return nil, nil }),
		Now:   func() time.Time { return now },
	})

	got, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
		panic("unreachable")
	}
	if len(got.ByAgentModel) != 2 {
		t.Fatalf("ByAgentModel = %+v, want 2 rows (direct review, conformant staff review)", got.ByAgentModel)
	}
	seen := map[string]int{}
	for _, row := range got.ByAgentModel {
		seen[row.SkillConformance]++
		if row.Runs != 1 {
			t.Fatalf("row = %+v, want 1 run each", row)
		}
	}
	if seen[SkillCohortSkill] != 1 || seen[SkillCohortDirect] != 1 {
		t.Fatalf("cohorts by conformance = %+v, want exactly one skill and one direct row", seen)
	}
}
