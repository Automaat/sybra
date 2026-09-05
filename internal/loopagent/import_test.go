package loopagent

import (
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestImport_KeepsIdsAndRunsOnce is what #3235 left undone: switching backends
// swapped the store and started from an empty table, so an operator's loop
// agents silently stopped running. The ids must survive too — the scheduler's
// run history and the GUI both key on them.
func TestImport_KeepsIdsAndRunsOnce(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		ctx := t.Context()
		dir := t.TempDir()
		fileStore, err := NewStore(dir)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		created, err := fileStore.Create(ctx, LoopAgent{Name: "nightly", Prompt: "run", IntervalSec: 3600})
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		for range 2 {
			if err := Import(ctx, d, dir, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := sqlStore.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("after two imports got %d agents, want 1", len(got))
		}
		if got[0].ID != created.ID {
			t.Errorf("import minted a new id %q, want %q", got[0].ID, created.ID)
		}
		if got[0].Name != "nightly" {
			t.Errorf("name = %q, want nightly", got[0].Name)
		}
		if _, err := fileStore.Get(ctx, created.ID); err != nil {
			t.Errorf("import removed the original file: %v", err)
		}
	})
}

func TestImport_EmptyDomainReadsBackEmpty(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if err := Import(t.Context(), d, t.TempDir(), "home-a", nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := store.List(t.Context())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d agents from an empty domain", len(got))
		}
	})
}
