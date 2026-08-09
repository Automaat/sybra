package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func importFixture(t *testing.T, dir string, defs []Definition) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i := range defs {
		def := &defs[i]
		data, err := yaml.Marshal(def)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, def.ID+".yaml"), data, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func definitionsFixture() []Definition {
	step := Step{ID: "s", Type: StepSetStatus, Config: StepConfig{Status: string(taskstatus.Todo)}}
	// Ids that carry punctuation, and the shipped builtins' own shapes. Under
	// glibc's en_US.UTF-8 the hyphen is ignored at the primary level, so
	// "pr-review" sorts after "prompt-lab-author" — the opposite of byte order
	// and of what the file store's directory listing gave. Ids like "wf-a"/
	// "wf-b" sort identically under every collation and can never fail here.
	return []Definition{
		{ID: "prompt-lab-author", Name: "author", Steps: []Step{step}},
		{ID: "pr-review", Name: "review", Steps: []Step{step}},
		{ID: "pr-fix", Name: "fix", Steps: []Step{step}},
		{ID: "branch-conflict-fix", Name: "conflict", Steps: []Step{step}},
		// A prefix pair. The file store sorts "<id>.yaml", so "wfx-a" comes
		// first ('-' < '.'); ordering by the bare id puts "wfx" first because a
		// prefix is shorter. Without this pair the parity assertion cannot see
		// the difference.
		{ID: "wfx", Name: "prefix", Steps: []Step{step}},
		{ID: "wfx-a", Name: "prefixed", Steps: []Step{step}},
	}
}

// TestSQLStore_ListMatchesTheFileStoreOrder is the issue's "same records in the
// same order". A table has no natural order, so a missing ORDER BY reads fine
// in one engine and reshuffles the workflow list in the other.
func TestSQLStore_ListMatchesTheFileStoreOrder(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		fileStore, err := NewStore(dir)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, def := range definitionsFixture() {
			if err := fileStore.Save(def); err != nil {
				t.Fatalf("file save: %v", err)
			}
			if err := sqlStore.Save(def); err != nil {
				t.Fatalf("sql save: %v", err)
			}
		}
		fromFiles, err := fileStore.List()
		if err != nil {
			t.Fatalf("file list: %v", err)
		}
		fromSQL, err := sqlStore.List()
		if err != nil {
			t.Fatalf("sql list: %v", err)
		}
		if len(fromSQL) != len(fromFiles) {
			t.Fatalf("sql listed %d, files listed %d", len(fromSQL), len(fromFiles))
		}
		for i := range fromFiles {
			if fromSQL[i].ID != fromFiles[i].ID {
				t.Fatalf("position %d: sql %q, files %q", i, fromSQL[i].ID, fromFiles[i].ID)
			}
		}
	})
}

// TestSQLStore_SnapshotIsImmutableAndKeyedByContent pins the snapshot contract
// the engine depends on: a task holds a hash and must get back exactly the
// definition it was dispatched against, however often the workflow has since
// been edited.
func TestSQLStore_SnapshotIsImmutableAndKeyedByContent(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		def := definitionsFixture()[0]
		hash, err := store.SaveSnapshot(def)
		if err != nil {
			t.Fatalf("save snapshot: %v", err)
		}
		again, err := store.SaveSnapshot(def)
		if err != nil {
			t.Fatalf("re-save snapshot: %v", err)
		}
		if again != hash {
			t.Fatalf("same definition hashed to %q then %q", hash, again)
		}
		got, err := store.GetSnapshot(def.ID, hash)
		if err != nil {
			t.Fatalf("get snapshot: %v", err)
		}
		if got.Name != def.Name {
			t.Errorf("snapshot name = %q, want %q", got.Name, def.Name)
		}

		// Editing the definition must leave the snapshot alone: a task mid-run
		// still resolves the steps it started with.
		edited := def
		edited.Name = "renamed"
		if err := store.Save(edited); err != nil {
			t.Fatalf("save edited: %v", err)
		}
		got, err = store.GetSnapshot(def.ID, hash)
		if err != nil {
			t.Fatalf("get snapshot after edit: %v", err)
		}
		if got.Name != def.Name {
			t.Errorf("snapshot followed the edit: name = %q, want %q", got.Name, def.Name)
		}
	})
}

// TestImport_IsOnceOnlyAndLeavesTheFiles pins the upgrade guarantees: import
// once, no duplicates on a second start, originals untouched, and an empty
// domain reads back empty rather than failing.
func TestImport_IsOnceOnlyAndLeavesTheFiles(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		defs := definitionsFixture()
		importFixture(t, dir, defs)

		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := store.List()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != len(defs) {
			t.Fatalf("after two imports got %d definitions, want %d", len(got), len(defs))
		}
		for _, def := range defs {
			if _, err := os.Stat(filepath.Join(dir, def.ID+".yaml")); err != nil {
				t.Errorf("import removed the original %s: %v", def.ID, err)
			}
		}
	})
}

func TestImport_EmptyDomainReadsBackEmpty(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if err := Import(t.Context(), d, filepath.Join(t.TempDir(), "never-written"), "home-a", nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := store.List()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d definitions from an empty domain", len(got))
		}
	})
}
