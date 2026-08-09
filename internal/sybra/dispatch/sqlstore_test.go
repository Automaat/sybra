package dispatch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func sqlController(t *testing.T, d *db.DB, owner string) *Controller {
	t.Helper()
	store, err := NewSQLPersistence(d)
	if err != nil {
		t.Fatalf("NewSQLPersistence: %v", err)
	}
	c, err := New(t.Context(), Options{
		Dir:    t.TempDir(),
		Owner:  owner,
		Store:  store,
		Limits: Limits{Global: 100},
		TTL:    time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// sqlIntent is a minimal admission intent for these tests. The package's own
// intent helper takes the full shape; only the identity matters here.
func sqlIntent(taskID string) agent.AttemptIntent {
	return agent.AttemptIntent{
		IntentID:            "intent-" + taskID,
		TaskID:              taskID,
		Provider:            providerid.Claude,
		Access:              agent.AttemptAccessObserve,
		CapabilityCertified: true,
	}
}

// TestSQLPersistence_ConcurrentAcquiresAreAllRecorded is the issue's "two
// concurrent updates to the same record cannot lose one of them".
//
// A lost lease is the costliest loss on the board: it releases work that is
// still running, so a second agent starts on a task another already holds.
func TestSQLPersistence_ConcurrentAcquiresAreAllRecorded(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		// One persistence per controller, because that is what two processes
		// are. Sharing a single one would let its process-local mutex serialize
		// them and the cross-process advisory lock would never be exercised —
		// the test would pass with the lock removed.
		storeA, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		storeB, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		one := sqlControllerWith(t, storeA, "owner-a")
		two := sqlControllerWith(t, storeB, "owner-b")

		const each = 25
		var wg sync.WaitGroup
		for i, c := range []*Controller{one, two} {
			wg.Go(func() {
				for n := range each {
					if _, err := c.Acquire(t.Context(), sqlIntent(taskName(i, n))); err != nil {
						t.Errorf("Acquire: %v", err)
						return
					}
				}
			})
		}
		wg.Wait()

		records, err := one.Records(t.Context())
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		if len(records) != 2*each {
			t.Fatalf("ledger holds %d leases, want %d; concurrent acquires lost some", len(records), 2*each)
		}
	})
}

func sqlControllerWith(t *testing.T, store Persistence, owner string) *Controller {
	t.Helper()
	c, err := New(t.Context(), Options{
		Dir: t.TempDir(), Owner: owner, Store: store,
		Limits: Limits{Global: 1000}, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func taskName(worker, n int) string {
	return "task-" + string(rune('a'+worker)) + "-" + string(rune('0'+n%10)) + string(rune('0'+n/10))
}

// TestSQLPersistence_LedgerSurvivesAndOrdersStably is the issue's "queue
// ordering after a restart matches the ordering before it".
func TestSQLPersistence_LedgerSurvivesAndOrdersStably(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		c := sqlController(t, d, "owner-a")
		for _, id := range []string{"pr-review", "prompt-lab", "pr-fix", "branch-conflict"} {
			if _, err := c.Acquire(t.Context(), sqlIntent(id)); err != nil {
				t.Fatalf("Acquire(%s): %v", id, err)
			}
		}
		first, err := c.Records(t.Context())
		if err != nil {
			t.Fatalf("Records: %v", err)
		}

		// A restart is a fresh controller over the same ledger.
		again := sqlController(t, d, "owner-a")
		second, err := again.Records(t.Context())
		if err != nil {
			t.Fatalf("Records after restart: %v", err)
		}
		if len(second) != len(first) {
			t.Fatalf("restart sees %d leases, want %d", len(second), len(first))
		}
		for i := range first {
			if first[i].ID != second[i].ID {
				t.Fatalf("position %d: before restart %q, after %q", i, first[i].ID, second[i].ID)
			}
		}
	})
}

// TestImport_CarriesInFlightLeases is the issue's "existing files import once,
// and in-flight leases survive the upgrade".
//
// Expiry has to come across as it stands: restamping it either releases every
// running agent's work at once, or holds a dead agent's claim past its timeout.
func TestImport_CarriesInFlightLeases(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		file, err := New(t.Context(), Options{
			Dir: dir, Owner: "owner-a", Limits: Limits{Global: 10}, TTL: time.Minute,
		})
		if err != nil {
			t.Fatalf("New(file): %v", err)
		}
		lease, err := file.Acquire(t.Context(), sqlIntent("task-a"))
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		before, err := file.Records(t.Context())
		if err != nil || len(before) != 1 {
			t.Fatalf("file ledger holds %d leases (%v)", len(before), err)
		}

		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		moved := sqlController(t, d, "owner-a")
		after, err := moved.Records(t.Context())
		if err != nil {
			t.Fatalf("Records: %v", err)
		}
		if len(after) != 1 {
			t.Fatalf("after two imports the ledger holds %d leases, want 1", len(after))
		}
		if after[0].ID != lease.ID {
			t.Errorf("lease id = %q, want %q", after[0].ID, lease.ID)
		}
		if !after[0].ExpiresAt.Equal(before[0].ExpiresAt) {
			t.Errorf("expiry moved from %s to %s; an in-flight lease was restamped",
				before[0].ExpiresAt, after[0].ExpiresAt)
		}
		if _, err := os.Stat(filepath.Join(dir, "attempt-leases.yaml")); err != nil {
			t.Errorf("import removed the original document: %v", err)
		}
	})
}

// TestSQLPersistence_ExpiredLeaseIsReclaimed is the issue's "a lease whose
// owner died is still reclaimed after its timeout".
func TestSQLPersistence_ExpiredLeaseIsReclaimed(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		clock := time.Now().UTC()
		c, err := New(t.Context(), Options{
			Dir: t.TempDir(), Owner: "dead-owner", Store: store,
			Limits: Limits{Global: 1}, TTL: time.Minute,
			Now: func() time.Time { return clock },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := c.Acquire(t.Context(), sqlIntent("task-a")); err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		// The owner dies; nothing heartbeats. A later start walks past the TTL,
		// which moves the lease to reconciling — still holding its slot, by
		// design, because an expired lease is a claim nobody has finalized.
		clock = clock.Add(10 * time.Minute)
		revived, err := New(t.Context(), Options{
			Dir: t.TempDir(), Owner: "new-owner", Store: store,
			Limits: Limits{Global: 1}, TTL: time.Minute,
			Now: func() time.Time { return clock },
		})
		if err != nil {
			t.Fatalf("New(revived): %v", err)
		}
		needs, err := revived.NeedsReconciliation(t.Context())
		if err != nil {
			t.Fatalf("NeedsReconciliation: %v", err)
		}
		if !needs {
			t.Fatal("the dead owner's expired lease was not marked for reconciliation, so nothing would ever reclaim it")
		}

		// Recovery observing no live agent is what finalizes it.
		n, err := revived.ReconcileUnobserved(t.Context(), nil)
		if err != nil {
			t.Fatalf("ReconcileUnobserved: %v", err)
		}
		if n != 1 {
			t.Fatalf("reconciled %d leases, want 1", n)
		}
		if _, err := revived.Acquire(t.Context(), sqlIntent("task-b")); err != nil {
			t.Fatalf("the slot stayed taken after the dead owner's lease was reconciled: %v", err)
		}
	})
}

// TestSQLPersistence_CriticalSectionsDoNotOverlap pins the property the ledger
// depends on, deterministically.
//
// The concurrency test above cannot: sqlite's immediate transaction serializes
// writers by itself, and on postgres the lost-update window between one
// instance's Load and another's Save is too narrow to hit on demand. So assert
// the mechanism instead — two instances, which is what two processes are, must
// never be inside a critical section at the same moment.
func TestSQLPersistence_CriticalSectionsDoNotOverlap(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		var (
			mu      sync.Mutex
			inside  int
			overlap bool
		)
		enter := func() {
			mu.Lock()
			defer mu.Unlock()
			inside++
			if inside > 1 {
				overlap = true
			}
		}
		leave := func() {
			mu.Lock()
			defer mu.Unlock()
			inside--
		}

		var wg sync.WaitGroup
		for range 4 {
			store, err := NewSQLPersistence(d)
			if err != nil {
				t.Fatalf("NewSQLPersistence: %v", err)
			}
			wg.Go(func() {
				for range 10 {
					if err := store.Critical(t.Context(), func() error {
						enter()
						// Long enough that an unserialized peer would be seen.
						time.Sleep(time.Millisecond)
						leave()
						return nil
					}); err != nil {
						t.Errorf("Critical: %v", err)
						return
					}
				}
			})
		}
		wg.Wait()

		if overlap {
			t.Fatal("two instances were inside a critical section at once; a read-modify-write can lose the other's update")
		}
	})
}

// TestSQLPersistence_SaveOutsideACriticalSectionIsRefused keeps a future caller
// from writing the ledger without the serialization that makes it safe.
func TestSQLPersistence_SaveOutsideACriticalSectionIsRefused(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLPersistence(d)
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		if err := store.Save(t.Context(), diskState{SchemaVersion: 1}); err == nil {
			t.Fatal("Save succeeded outside a critical section; nothing serialized it")
		}
	})
}
