package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLimitsFile(t *testing.T, path string, p persisted) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStore_ReloadPrunesExpiredEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	writeLimitsFile(t, path, persisted{
		Events: []UsageEvent{
			{ID: "old", Provider: ProviderClaude, Timestamp: now.Add(-eventMaxAge - time.Hour)},
			{ID: "fresh", Provider: ProviderClaude, Timestamp: now.Add(-time.Hour)},
			{ID: "boundary", Provider: ProviderClaude, Timestamp: now.Add(-eventMaxAge + time.Hour)},
		},
	})

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if err := s.reloadLocked(); err != nil {
		t.Fatal(err)
	}

	if len(s.events) != 2 {
		t.Fatalf("expected 2 surviving events, got %d: %+v", len(s.events), s.events)
	}
	ids := map[string]bool{}
	for _, e := range s.events {
		ids[e.ID] = true
	}
	if ids["old"] {
		t.Fatal("expired event was not pruned")
	}
	if !ids["fresh"] || !ids["boundary"] {
		t.Fatalf("unexpected surviving events: %+v", s.events)
	}
}

func TestStore_RecordUsageFlushesPrunedEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	writeLimitsFile(t, path, persisted{
		Events: []UsageEvent{
			{ID: "old", Provider: ProviderClaude, Timestamp: now.Add(-eventMaxAge - time.Hour)},
		},
	})

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }

	if err := s.RecordUsage(UsageEvent{ID: "new", Provider: ProviderClaude, Timestamp: now}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 1 || p.Events[0].ID != "new" {
		t.Fatalf("expected only the fresh event on disk, got %+v", p.Events)
	}
}

func TestStore_InvalidateLiveExactSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 22, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.UpdateSnapshot(Snapshot{
		Provider:   ProviderClaude,
		Source:     SourceLivePoll,
		Confidence: ConfidenceExact,
		CapturedAt: now,
		Primary:    &CycleSnapshot{UsedPercent: 88, WindowMinutes: 300, ResetsAt: now.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.InvalidateLiveExactSnapshot(ProviderClaude); err != nil {
		t.Fatalf("InvalidateLiveExactSnapshot: %v", err)
	}

	snap, ok := s.Snapshot(ProviderClaude)
	if !ok {
		t.Fatal("snapshot missing after invalidation")
	}
	if snap.Confidence != ConfidenceEstimated {
		t.Fatalf("confidence = %q, want estimated", snap.Confidence)
	}
	if snap.Primary == nil || snap.Primary.UsedPercent != 88 {
		t.Fatalf("primary cycle changed unexpectedly: %+v", snap.Primary)
	}
}
