package evaluation

import (
	"context"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
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
	if len(got.ByVariant) != 1 {
		t.Fatalf("ByVariant rows = %d, want 1 aggregate parent: %+v", len(got.ByVariant), got.ByVariant)
	}
	parent := got.ByVariant[0]
	if parent.ExperimentID != "exp" || parent.VariantID != "a" || parent.Role != "" || parent.Runs != 2 || parent.Landed != 2 {
		t.Fatalf("ByVariant parent = %+v, want aggregate exp/a row across roles", parent)
	}
	if len(parent.RoleBreakdowns) != 2 {
		t.Fatalf("roleBreakdowns = %d, want 2: %+v", len(parent.RoleBreakdowns), parent.RoleBreakdowns)
	}
	if got.ByAgentModel[0].RoleBreakdowns != nil {
		t.Fatalf("ByAgentModel should remain flat, got nested rows: %+v", got.ByAgentModel[0])
	}
}
