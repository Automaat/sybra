package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func writeAuditFile(t *testing.T, dir, filename string, evts []audit.Event) {
	t.Helper()
	var lines []byte
	for _, e := range evts {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, b...)
		lines = append(lines, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, filename), lines, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T) (store *Store, tmpDir string) {
	t.Helper()
	tmpDir = t.TempDir()
	s, err := NewStore(filepath.Join(tmpDir, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s, tmpDir
}

func newAuditDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBackfillEmptyDir(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Errorf("expected 0 runs, got %d", s.Len())
	}
}

func TestBackfillNonExistentDir(t *testing.T) {
	s, dir := newTestStore(t)

	// audit.Read returns nil events for missing dirs, not an error.
	if err := s.Backfill(filepath.Join(dir, "nonexistent")); err != nil {
		t.Errorf("expected no error for missing dir, got %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("expected 0 runs, got %d", s.Len())
	}
}

func TestBackfillImportsCompletedAndFailed(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	writeAuditFile(t, auditDir, "2024-01-15.ndjson", []audit.Event{
		{
			Timestamp: ts,
			Type:      audit.EventAgentCompleted,
			TaskID:    "t1",
			AgentID:   "a1",
			Data:      map[string]any{"mode": "headless", "cost_usd": 0.05, "duration_s": 30.0, "state": "stopped"},
		},
		{
			Timestamp: ts.Add(time.Hour),
			Type:      audit.EventAgentFailed,
			TaskID:    "t2",
			AgentID:   "a2",
			Data:      map[string]any{"mode": "interactive", "cost_usd": 0.02, "duration_s": 10.0, "state": "error"},
		},
	})

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}

	if s.Len() != 2 {
		t.Fatalf("expected 2 runs, got %d", s.Len())
	}

	resp := s.Query()
	if resp.AllTime.TotalRuns != 2 {
		t.Errorf("totalRuns: got %d, want 2", resp.AllTime.TotalRuns)
	}

	wantCost := 0.05 + 0.02
	if diff := resp.AllTime.TotalCostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("totalCost: got %f, want %f", resp.AllTime.TotalCostUSD, wantCost)
	}
}

func TestBackfillSkipsIfStoreHasRecords(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	if err := s.Record(RunRecord{ID: "existing", Outcome: "completed", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	writeAuditFile(t, auditDir, "2024-01-15.ndjson", []audit.Event{
		{Timestamp: ts, Type: audit.EventAgentCompleted, AgentID: "a1", Data: map[string]any{"state": "stopped"}},
	})

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}

	if s.Len() != 1 {
		t.Errorf("expected 1 (no backfill), got %d", s.Len())
	}
}

func TestBackfillFiltersNonAgentEvents(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	ts := time.Date(2024, 2, 1, 10, 0, 0, 0, time.UTC)
	writeAuditFile(t, auditDir, "2024-02-01.ndjson", []audit.Event{
		{Timestamp: ts, Type: audit.EventTaskCreated, TaskID: "t1"},
		{Timestamp: ts, Type: audit.EventAgentStarted, AgentID: "a1"},
		{Timestamp: ts, Type: audit.EventAgentCompleted, AgentID: "a2", Data: map[string]any{"state": "stopped"}},
		{Timestamp: ts, Type: audit.EventPlanCompleted, TaskID: "t2"},
	})

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}

	if s.Len() != 1 {
		t.Errorf("expected 1 run (only agent.completed), got %d", s.Len())
	}
}

func TestBackfillMalformedEntries(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	content := []byte(`{"ts":"2024-03-01T10:00:00Z","type":"agent.completed","agent_id":"a1","data":{"state":"stopped"}}
{invalid json line
{"ts":"2024-03-01T11:00:00Z","type":"agent.completed","agent_id":"a2","data":{"state":"stopped"}}
`)
	if err := os.WriteFile(filepath.Join(auditDir, "2024-03-01.ndjson"), content, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}

	if s.Len() != 2 {
		t.Errorf("expected 2 runs (malformed line skipped), got %d", s.Len())
	}
}

func TestBackfillOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		eventType   string
		state       string
		wantOutcome string
	}{
		{"completed_stopped", audit.EventAgentCompleted, "stopped", "completed"},
		{"completed_other_state", audit.EventAgentCompleted, "error", "failed"},
		{"failed_event", audit.EventAgentFailed, "error", "failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			auditDir := newAuditDir(t)

			ts := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
			writeAuditFile(t, auditDir, "2024-03-01.ndjson", []audit.Event{
				{
					Timestamp: ts,
					Type:      tc.eventType,
					AgentID:   "a1",
					Data:      map[string]any{"state": tc.state},
				},
			})

			if err := s.Backfill(auditDir); err != nil {
				t.Fatal(err)
			}

			resp := s.Query()
			if len(resp.RecentRuns) != 1 {
				t.Fatalf("expected 1 run, got %d", len(resp.RecentRuns))
			}
			if resp.RecentRuns[0].Outcome != tc.wantOutcome {
				t.Errorf("outcome: got %s, want %s", resp.RecentRuns[0].Outcome, tc.wantOutcome)
			}
		})
	}
}

func TestBackfillFieldExtraction(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	ts := time.Date(2024, 5, 1, 8, 30, 0, 0, time.UTC)
	writeAuditFile(t, auditDir, "2024-05-01.ndjson", []audit.Event{
		{
			Timestamp: ts,
			Type:      audit.EventAgentCompleted,
			TaskID:    "task-xyz",
			AgentID:   "agent-abc",
			Data: map[string]any{
				"mode":       "headless",
				"cost_usd":   0.12,
				"duration_s": 45.5,
				"state":      "stopped",
			},
		},
	})

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}

	resp := s.Query()
	if len(resp.RecentRuns) != 1 {
		t.Fatalf("expected 1 run, got %d", len(resp.RecentRuns))
	}

	r := resp.RecentRuns[0]
	if r.ID != "agent-abc" {
		t.Errorf("ID: got %s, want agent-abc", r.ID)
	}
	if r.TaskID != "task-xyz" {
		t.Errorf("TaskID: got %s, want task-xyz", r.TaskID)
	}
	if r.Mode != "headless" {
		t.Errorf("Mode: got %s, want headless", r.Mode)
	}
	if r.CostUSD != 0.12 {
		t.Errorf("CostUSD: got %f, want 0.12", r.CostUSD)
	}
	if r.DurationS != 45.5 {
		t.Errorf("DurationS: got %f, want 45.5", r.DurationS)
	}
	if r.Outcome != "completed" {
		t.Errorf("Outcome: got %s, want completed", r.Outcome)
	}
	if !r.Timestamp.Equal(ts) {
		t.Errorf("Timestamp: got %v, want %v", r.Timestamp, ts)
	}
}

func TestBackfillMultipleFiles(t *testing.T) {
	s, _ := newTestStore(t)
	auditDir := newAuditDir(t)

	writeAuditFile(t, auditDir, "2024-06-01.ndjson", []audit.Event{
		{Timestamp: time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC), Type: audit.EventAgentCompleted, AgentID: "a1", Data: map[string]any{"state": "stopped"}},
	})
	writeAuditFile(t, auditDir, "2024-06-02.ndjson", []audit.Event{
		{Timestamp: time.Date(2024, 6, 2, 10, 0, 0, 0, time.UTC), Type: audit.EventAgentCompleted, AgentID: "a2", Data: map[string]any{"state": "stopped"}},
		{Timestamp: time.Date(2024, 6, 2, 11, 0, 0, 0, time.UTC), Type: audit.EventAgentFailed, AgentID: "a3", Data: map[string]any{"state": "error"}},
	})

	if err := s.Backfill(auditDir); err != nil {
		t.Fatal(err)
	}

	if s.Len() != 3 {
		t.Errorf("expected 3 runs across 2 files, got %d", s.Len())
	}
}
