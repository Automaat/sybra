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
		store, err := NewSQLPersistence(d)
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
	for _, op := range ops {
		out = append(out, op.ID)
	}
	return out
}

// TestImport_IsOnceOnlyAndLeavesTheFile pins the upgrade guarantees for the one
// document this domain keeps.
func TestImport_IsOnceOnlyAndLeavesTheFile(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		path := filepath.Join(t.TempDir(), "bgops.json")
		data, err := json.MarshalIndent(opsFixture(), "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		for range 2 {
			if err := Import(t.Context(), d, path, nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLPersistence(d)
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
		if err := Import(t.Context(), d, filepath.Join(t.TempDir(), "never-written.json"), nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLPersistence(d)
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
