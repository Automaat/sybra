package evaluation

import (
	"context"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
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
	}
	if len(got.ByExperimentKind) != 0 {
		t.Fatalf("ByExperimentKind = %+v, want empty for records with no experiment metadata", got.ByExperimentKind)
	}
	if len(got.ByAgentModel) == 0 {
		t.Fatalf("ByAgentModel should still populate from non-experiment records")
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
