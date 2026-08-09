package monitor

import (
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func sqlIncidentStore(t *testing.T, d *db.DB) *IncidentStore {
	t.Helper()
	backend, err := NewSQLIncidentStore(d)
	if err != nil {
		t.Fatalf("NewSQLIncidentStore: %v", err)
	}
	store, err := NewIncidentStoreWith(t.TempDir(), backend)
	if err != nil {
		t.Fatalf("NewIncidentStoreWith: %v", err)
	}
	return store
}

// TestSQLIncidentStore_LockedCyclesDoNotOverlap is the issue's "two concurrent
// updates to the same record cannot lose one of them".
//
// An incident cycle is read-modify-write, so two instances inside one at the
// same moment can drop an observation — or re-file an issue already filed.
// Asserted on the mechanism because the lost-update window itself is too narrow
// to hit on demand.
func TestSQLIncidentStore_LockedCyclesDoNotOverlap(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		var (
			mu      sync.Mutex
			inside  int
			overlap bool
		)
		var wg sync.WaitGroup
		for range 4 {
			backend, err := NewSQLIncidentStore(d)
			if err != nil {
				t.Fatalf("NewSQLIncidentStore: %v", err)
			}
			wg.Go(func() {
				for range 8 {
					release, err := backend.Lock()
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
					if err := release(); err != nil {
						t.Errorf("release: %v", err)
						return
					}
				}
			})
		}
		wg.Wait()
		if overlap {
			t.Fatal("two instances held the incident ledger at once; an observation can be lost")
		}
	})
}

// TestSQLIncidentStore_SaveOutsideALockIsRefused keeps a later caller from
// writing an incident without the serialization that makes the cycle safe.
func TestSQLIncidentStore_SaveOutsideALockIsRefused(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		backend, err := NewSQLIncidentStore(d)
		if err != nil {
			t.Fatalf("NewSQLIncidentStore: %v", err)
		}
		if err := backend.Save(Incident{Fingerprint: "fp-1"}); err == nil {
			t.Fatal("Save succeeded outside a locked cycle; nothing serialized it")
		}
	})
}

// TestSQLIncidentStore_RoundTripsThroughTheStore pins that an incident written
// through the public store reads back, which every monitor pass depends on.
func TestSQLIncidentStore_RoundTripsThroughTheStore(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store := sqlIncidentStore(t, d)
		got, err := store.List()
		if err != nil {
			t.Fatalf("List on an empty ledger: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty ledger returned %d incidents", len(got))
		}

		backend, err := NewSQLIncidentStore(d)
		if err != nil {
			t.Fatalf("NewSQLIncidentStore: %v", err)
		}
		release, err := backend.Lock()
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		in := Incident{Fingerprint: "fp-1", FailureCode: "boom", LastSeen: time.Now().UTC()}
		if err := backend.Save(in); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := release(); err != nil {
			t.Fatalf("release: %v", err)
		}

		back, ok, err := backend.Load("fp-1")
		if err != nil || !ok {
			t.Fatalf("Load: ok=%v err=%v", ok, err)
		}
		if back.FailureCode != "boom" {
			t.Errorf("failure code = %q, want boom", back.FailureCode)
		}
	})
}
