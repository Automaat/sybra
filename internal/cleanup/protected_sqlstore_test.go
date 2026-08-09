package cleanup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func findingsFixture() protectedFile {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return protectedFile{Findings: []Finding{
		{ID: "wt-b", Kind: ResourceWorktree, Path: "/tmp/b", Reason: "held", FirstSeenAt: base.Add(time.Hour)},
		{ID: "wt-a", Kind: ResourceWorktree, Path: "/tmp/a", Reason: "held", FirstSeenAt: base},
	}}
}

// TestSQLProtectedStore_RoundTripsInAStableOrder pins that a finding survives
// and that a pass reads them in one order, which the JSON document's own sorted
// slice gave.
func TestSQLProtectedStore_RoundTripsInAStableOrder(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLProtectedStore(d)
		if err != nil {
			t.Fatalf("NewSQLProtectedStore: %v", err)
		}
		release, err := store.Lock()
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		if err := store.Write(findingsFixture()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		release()

		got, err := store.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(got.Findings) != 2 {
			t.Fatalf("read %d findings, want 2", len(got.Findings))
		}
		if got.Findings[0].ID != "wt-a" {
			t.Errorf("order starts at %q, want wt-a", got.Findings[0].ID)
		}
	})
}

// TestSQLProtectedStore_WriteOutsideALockIsRefused keeps a later caller from
// replacing the ledger without the serialization that makes the cycle safe.
func TestSQLProtectedStore_WriteOutsideALockIsRefused(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLProtectedStore(d)
		if err != nil {
			t.Fatalf("NewSQLProtectedStore: %v", err)
		}
		if err := store.Write(findingsFixture()); err == nil {
			t.Fatal("Write succeeded outside a locked cycle; nothing serialized it")
		}
	})
}

// TestSQLProtectedStore_LockedCyclesDoNotOverlap is the issue's "two concurrent
// updates to the same record cannot lose one of them". Losing a finding is what
// lets a later cleanup pass delete the path it protects.
func TestSQLProtectedStore_LockedCyclesDoNotOverlap(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		var (
			mu      sync.Mutex
			inside  int
			overlap bool
		)
		var wg sync.WaitGroup
		for range 4 {
			store, err := NewSQLProtectedStore(d)
			if err != nil {
				t.Fatalf("NewSQLProtectedStore: %v", err)
			}
			wg.Go(func() {
				for range 8 {
					release, err := store.Lock()
					if err != nil {
						t.Errorf("Lock: %v", err)
						return
					}
					mu.Lock()
					inside++
					if inside > 1 {
						overlap = true
					}
					mu.Unlock()
					time.Sleep(time.Millisecond)
					mu.Lock()
					inside--
					mu.Unlock()
					release()
				}
			})
		}
		wg.Wait()
		if overlap {
			t.Fatal("two instances held the findings ledger at once; a protected path can be forgotten")
		}
	})
}

// TestImportProtected_IsOnceOnlyAndLeavesTheFile pins the upgrade guarantees.
func TestImportProtected_IsOnceOnlyAndLeavesTheFile(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "protected-findings.json")
		data, err := json.MarshalIndent(findingsFixture(), "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		for range 2 {
			if err := ImportProtected(t.Context(), d, path, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLProtectedStore(d)
		if err != nil {
			t.Fatalf("NewSQLProtectedStore: %v", err)
		}
		got, err := store.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(got.Findings) != 2 {
			t.Fatalf("after two imports the ledger holds %d findings, want 2", len(got.Findings))
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("import removed the original document: %v", err)
		}
	})
}
