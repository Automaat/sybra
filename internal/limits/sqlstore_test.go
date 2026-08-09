package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func quotaFixture(now time.Time) (map[string]Snapshot, []UsageEvent) {
	snapshots := map[string]Snapshot{
		providerid.Claude: {Provider: providerid.Claude, PlanType: "max", CapturedAt: now.Add(-time.Minute)},
		providerid.Codex:  {Provider: providerid.Codex, PlanType: "pro", CapturedAt: now.Add(-2 * time.Minute)},
	}
	events := []UsageEvent{
		{ID: "e1", Provider: providerid.Claude, Source: "run", CostUSD: 1, Timestamp: now.Add(-time.Hour)},
		{ID: "e2", Provider: providerid.Codex, Source: "run", CostUSD: 2, Timestamp: now.Add(-30 * time.Minute)},
	}
	return snapshots, events
}

// TestSQLPersistence_RoundTrips pins that what the store saves is what it loads back, which is what every provider-selection decision reads.
func TestSQLPersistence_RoundTrips(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		now := time.Now().UTC()
		snapshots, events := quotaFixture(now)
		if err := store.Save(snapshots, events); err != nil {
			t.Fatalf("save: %v", err)
		}
		gotSnapshots, gotEvents, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(gotSnapshots) != 2 || gotSnapshots[providerid.Claude].PlanType != "max" {
			t.Fatalf("snapshots round-tripped as %+v", gotSnapshots)
		}
		if len(gotEvents) != 2 || gotEvents[0].ID != "e1" {
			t.Fatalf("events round-tripped as %+v", gotEvents)
		}

		// Every update hands over the whole set, so a repeat must not multiply what is already stored.
		if err := store.Save(snapshots, events); err != nil {
			t.Fatalf("second save: %v", err)
		}
		_, gotEvents, err = store.Load()
		if err != nil {
			t.Fatalf("second load: %v", err)
		}
		if len(gotEvents) != 2 {
			t.Fatalf("re-saving produced %d events, want 2", len(gotEvents))
		}
	})
}

// TestSQLPersistence_PrunesEventsPastRetention is the issue's "retention still removes data past its configured age".
func TestSQLPersistence_PrunesEventsPastRetention(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		now := time.Now().UTC()
		events := []UsageEvent{
			{ID: "old", Provider: providerid.Claude, Timestamp: now.Add(-eventMaxAge - time.Hour)},
			{ID: "fresh", Provider: providerid.Claude, Timestamp: now.Add(-time.Hour)},
		}
		if err := store.Save(map[string]Snapshot{}, events); err != nil {
			t.Fatalf("save: %v", err)
		}
		_, got, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(got) != 1 || got[0].ID != "fresh" {
			t.Fatalf("retention kept %+v, want only the fresh event", got)
		}
	})
}

// TestImport_IsOnceOnlyAndLeavesTheFile pins the upgrade guarantees.
func TestImport_IsOnceOnlyAndLeavesTheFile(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		now := time.Now().UTC()
		snapshots, events := quotaFixture(now)
		path := filepath.Join(t.TempDir(), "limits.json")
		data, err := json.Marshal(persisted{Snapshots: snapshots, Events: events})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		for range 2 {
			if err := Import(t.Context(), d, path, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		gotSnapshots, gotEvents, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(gotSnapshots) != 2 || len(gotEvents) != 2 {
			t.Fatalf("after two imports got %d snapshots and %d events, want 2 and 2", len(gotSnapshots), len(gotEvents))
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("import removed the original document: %v", err)
		}
	})
}

func TestImport_EmptyDomainReadsBackEmpty(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if err := Import(t.Context(), d, filepath.Join(t.TempDir(), "never-written.json"), "home-a", nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		snapshots, events, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(snapshots) != 0 || len(events) != 0 {
			t.Fatalf("got %d snapshots and %d events from an empty domain", len(snapshots), len(events))
		}
	})
}

// TestSQLPersistence_ConcurrentInstancesDoNotLoseSnapshots is the case a shared
// board exists for.
//
// The cross-process file lock made load-mutate-save atomic. Replacing it with a
// no-op left two instances interleaving as load/load/save/save, and because
// Save replaces the snapshot set, the second write dropped the first
// instance's provider entirely — a provider that is actually rate-limited then
// reads back as untouched headroom and dispatch routes into it.
func TestSQLPersistence_ConcurrentInstancesDoNotLoseSnapshots(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		persistence, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		one, err := NewStoreWith(persistence)
		if err != nil {
			t.Fatalf("NewStoreWith: %v", err)
		}
		two, err := NewStoreWith(persistence)
		if err != nil {
			t.Fatalf("NewStoreWith: %v", err)
		}

		const rounds = 60
		var wg sync.WaitGroup
		for _, pair := range []struct {
			store    *Store
			provider string
		}{{one, providerid.Claude}, {two, providerid.Codex}} {
			wg.Go(func() {
				for i := range rounds {
					if err := pair.store.UpdateSnapshot(Snapshot{
						Provider:   pair.provider,
						PlanType:   "max",
						CapturedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
					}); err != nil {
						t.Errorf("UpdateSnapshot(%s): %v", pair.provider, err)
						return
					}
				}
			})
		}
		wg.Wait()

		got, _, err := persistence.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		for _, provider := range []string{providerid.Claude, providerid.Codex} {
			if _, ok := got[provider]; !ok {
				t.Fatalf("%s's snapshot was lost to the other instance; board holds %v", provider, keys(got))
			}
		}
	})
}

func keys(m map[string]Snapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
