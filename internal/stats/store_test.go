package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewStoreEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 0 {
		t.Fatalf("expected 0 runs, got %d", s.Len())
	}
}

func TestRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	runs := []RunRecord{
		{
			ID: "a1", TaskID: "t1", ProjectID: "org/repo",
			Mode: "headless", Role: "implementation", Model: "sonnet",
			CostUSD: 0.05, DurationS: 30, InputTokens: 1000, OutputTokens: 500,
			Outcome: "completed", Timestamp: now,
		},
		{
			ID: "a2", TaskID: "t2", ProjectID: "org/repo",
			Mode: "interactive", Role: "triage", Model: "opus",
			CostUSD: 0.10, DurationS: 60, InputTokens: 2000, OutputTokens: 1000,
			Outcome: "completed", Timestamp: now.Add(-time.Hour),
		},
		{
			ID: "a3", TaskID: "t3", ProjectID: "org/other",
			Mode: "headless", Role: "plan", Model: "sonnet",
			CostUSD: 0.03, DurationS: 20, InputTokens: 500, OutputTokens: 200,
			Outcome: "failed", Timestamp: now.Add(-48 * time.Hour),
		},
	}

	for _, r := range runs {
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}

	if s.Len() != 3 {
		t.Fatalf("expected 3 runs, got %d", s.Len())
	}

	resp := s.Query()

	// AllTime
	if resp.AllTime.TotalRuns != 3 {
		t.Errorf("allTime.totalRuns: got %d, want 3", resp.AllTime.TotalRuns)
	}
	wantCost := 0.05 + 0.10 + 0.03
	if diff := resp.AllTime.TotalCostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("allTime.totalCost: got %f, want %f", resp.AllTime.TotalCostUSD, wantCost)
	}
	if resp.AllTime.TotalInputTokens != 3500 {
		t.Errorf("allTime.totalInputTokens: got %d, want 3500", resp.AllTime.TotalInputTokens)
	}

	// ByProject sorted by cost desc
	if len(resp.ByProject) != 2 {
		t.Fatalf("byProject: got %d groups, want 2", len(resp.ByProject))
	}
	if resp.ByProject[0].Key != "org/repo" {
		t.Errorf("byProject[0].key: got %s, want org/repo", resp.ByProject[0].Key)
	}

	// ByMode
	if len(resp.ByMode) != 2 {
		t.Errorf("byMode: got %d groups, want 2", len(resp.ByMode))
	}

	// RecentRuns newest first
	if len(resp.RecentRuns) != 3 {
		t.Fatalf("recentRuns: got %d, want 3", len(resp.RecentRuns))
	}
	if resp.RecentRuns[0].ID != "a1" {
		t.Errorf("recentRuns[0].id: got %s, want a1", resp.RecentRuns[0].ID)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	r := RunRecord{
		ID: "a1", TaskID: "t1", Mode: "headless", Role: "implementation",
		CostUSD: 0.05, DurationS: 30, Outcome: "completed",
		Timestamp: time.Now(),
	}
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}

	// Verify file exists
	if _, err := os.Stat(path); err != nil {
		t.Fatal("stats file not created")
	}

	// Reload from disk
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 1 {
		t.Fatalf("after reload: expected 1 run, got %d", s2.Len())
	}

	resp := s2.Query()
	if resp.AllTime.TotalCostUSD != 0.05 {
		t.Errorf("after reload: totalCost got %f, want 0.05", resp.AllTime.TotalCostUSD)
	}
}

func TestQueryEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	resp := s.Query()
	if resp.AllTime.TotalRuns != 0 {
		t.Errorf("empty store: expected 0 runs, got %d", resp.AllTime.TotalRuns)
	}
	if len(resp.RecentRuns) != 0 {
		t.Errorf("empty store: expected empty recentRuns, got %d", len(resp.RecentRuns))
	}
}
