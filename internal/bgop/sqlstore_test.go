package bgop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func opsFixture() []Operation {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return []Operation{
		{ID: "op-a", Type: TypeClone, Status: StatusDone, StartedAt: base, CompletedAt: base.Add(time.Minute), Label: "first"},
		{ID: "op-b", Type: TypeWorktreePrep, Status: StatusDone, StartedAt: base.Add(time.Hour), CompletedAt: base.Add(time.Hour), Label: "second"},
	}
}

// TestSQLPersistence_SaveReplacesTheSet pins the semantics the tracker has
// always had: it hands over its whole set, and an operation it dropped for age
// is gone by being absent rather than by an explicit delete.
func TestSQLPersistence_SaveReplacesTheSet(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLPersistence(d, "instance-a")
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		if err := store.Save(opsFixture()); err != nil {
			t.Fatalf("save: %v", err)
		}
		got, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(got) != 2 || got[0].ID != "op-a" || got[1].ID != "op-b" {
			t.Fatalf("load returned %v, want op-a then op-b", ids(got))
		}

		if err := store.Save(opsFixture()[:1]); err != nil {
			t.Fatalf("second save: %v", err)
		}
		got, err = store.Load()
		if err != nil {
			t.Fatalf("second load: %v", err)
		}
		if len(got) != 1 || got[0].ID != "op-a" {
			t.Fatalf("dropped operation survived: %v", ids(got))
		}
	})
}

func ids(ops []Operation) []string {
	out := make([]string, 0, len(ops))
	for i := range ops {
		out = append(out, ops[i].ID)
	}
	return out
}

// TestImport_IsOnceOnlyAndLeavesTheFile pins the upgrade guarantees for the one
// document this domain keeps.
func TestImport_IsOnceOnlyAndLeavesTheFile(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "bgops.json")
		data, err := json.MarshalIndent(opsFixture(), "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		for range 2 {
			if err := Import(t.Context(), d, path, "instance-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLPersistence(d, "instance-a")
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		got, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("after two imports got %d operations, want 2: %v", len(got), ids(got))
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("import removed the original document: %v", err)
		}
	})
}

func TestImport_EmptyDomainReadsBackEmpty(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if err := Import(t.Context(), d, filepath.Join(t.TempDir(), "never-written.json"), "instance-a", nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLPersistence(d, "instance-a")
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		got, err := store.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d operations from an empty domain", len(got))
		}
	})
}

// TestSQLPersistence_InstancesDoNotClobberEachOther is the shared-board case a
// postgres backend exists for.
//
// Each tracker hands over its whole set on every change, so a store that
// replaced every row would make one machine's next Start delete every
// operation the other machines were running — and the operator would watch
// their progress panel empty itself for no visible reason.
func TestSQLPersistence_InstancesDoNotClobberEachOther(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		storeA, err := NewSQLPersistence(d, "instance-a")
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		storeB, err := NewSQLPersistence(d, "instance-b")
		if err != nil {
			t.Fatalf("NewSQLPersistence: %v", err)
		}
		trackerA := NewTracker(func(string, any) {}, storeA, nil)
		trackerB := NewTracker(func(string, any) {}, storeB, nil)

		trackerA.Start(TypeClone, "machine A clone", "o/r", "task-a")
		trackerB.Start(TypeClone, "machine B clone", "o/r", "task-b")

		ownedByA, err := storeA.Load()
		if err != nil {
			t.Fatalf("load A: %v", err)
		}
		if len(ownedByA) != 1 || ownedByA[0].Label != "machine A clone" {
			t.Fatalf("instance A sees %v, want only its own operation", ids(ownedByA))
		}
		ownedByB, err := storeB.Load()
		if err != nil {
			t.Fatalf("load B: %v", err)
		}
		if len(ownedByB) != 1 || ownedByB[0].Label != "machine B clone" {
			t.Fatalf("instance B lost its operation to the other instance: %v", ids(ownedByB))
		}
	})
}

// TestNewSQLPersistence_RefusesAnEmptyOwner keeps every instance from sharing
// one blank scope, which is the clobbering case again with no symptom to
// notice it by.
func TestNewSQLPersistence_RefusesAnEmptyOwner(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if _, err := NewSQLPersistence(d, "  "); err == nil {
			t.Fatal("accepted a blank owner; every instance would share one scope")
		}
	})
}
