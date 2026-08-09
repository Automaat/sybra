package experience

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func seedRecordFiles(t *testing.T, dir, projectKey string, recs []Record) {
	t.Helper()
	projectDir := filepath.Join(dir, projectKey)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, rec := range recs {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, rec.TaskID+".json"), append(data, '\n'), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func recordsFixture() []Record {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return []Record{
		{TaskID: "task-a", CreatedAt: base, ProjectID: "o/r", Title: "oldest"},
		{TaskID: "task-c", CreatedAt: base.Add(2 * time.Hour), ProjectID: "o/r", Title: "newest"},
		{TaskID: "task-b", CreatedAt: base.Add(time.Hour), ProjectID: "o/r", Title: "middle"},
	}
}

// TestSQLStore_QueryMatchesTheFileStoreOrder is the issue's "same records in
// the same order" outcome. A table has no natural order, so an omitted ORDER BY
// reads correct in a unit test and reorders an agent's advisory context in
// production.
func TestSQLStore_QueryMatchesTheFileStoreOrder(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		dir := t.TempDir()
		fileStore, err := New(dir)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, rec := range recordsFixture() {
			if err := fileStore.Put("o/r", rec); err != nil {
				t.Fatalf("file put: %v", err)
			}
			if err := sqlStore.Put("o/r", rec); err != nil {
				t.Fatalf("sql put: %v", err)
			}
		}
		fromFiles, err := fileStore.Query("o/r", 10)
		if err != nil {
			t.Fatalf("file query: %v", err)
		}
		fromSQL, err := sqlStore.Query("o/r", 10)
		if err != nil {
			t.Fatalf("sql query: %v", err)
		}
		if len(fromSQL) != len(fromFiles) {
			t.Fatalf("sql returned %d records, files returned %d", len(fromSQL), len(fromFiles))
		}
		for i := range fromFiles {
			if fromSQL[i].TaskID != fromFiles[i].TaskID {
				t.Fatalf("position %d: sql %q, files %q (sql=%v files=%v)",
					i, fromSQL[i].TaskID, fromFiles[i].TaskID, ids(fromSQL), ids(fromFiles))
			}
		}
	})
}

func ids(recs []Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.TaskID)
	}
	return out
}

// TestImport_IsOnceOnlyAndLeavesTheFiles pins the three upgrade guarantees the
// issue asks for: files import once, a second start does not duplicate them,
// and the originals stay on disk.
func TestImport_IsOnceOnlyAndLeavesTheFiles(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		dir := t.TempDir()
		// The directory name the file store would have written, so the import
		// reads exactly what an upgrading install has on disk.
		projectKey, err := fsutil.ProjectKeyDir("o/r")
		if err != nil {
			t.Fatalf("ProjectKeyDir: %v", err)
		}
		seedRecordFiles(t, dir, projectKey, recordsFixture())

		for i := range 2 {
			if err := Import(t.Context(), d, dir, nil); err != nil {
				t.Fatalf("import %d: %v", i, err)
			}
		}

		store, storeErr := NewSQLStore(d)
		if storeErr != nil {
			t.Fatalf("NewSQLStore: %v", storeErr)
		}
		got, err := store.Query("o/r", 100)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("after two imports got %d records, want 3: %v", len(got), ids(got))
		}
		if got[0].TaskID != "task-c" {
			t.Errorf("newest record is %q, want task-c: %v", got[0].TaskID, ids(got))
		}
		for _, rec := range recordsFixture() {
			path := filepath.Join(dir, projectKey, rec.TaskID+".json")
			if _, err := os.Stat(path); err != nil {
				t.Errorf("import removed the original %s: %v", path, err)
			}
		}
	})
}

// TestImport_EmptyDomainReadsBackEmpty is the issue's "reading a domain with no
// records yet returns empty rather than failing".
func TestImport_EmptyDomainReadsBackEmpty(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		if err := Import(t.Context(), d, filepath.Join(t.TempDir(), "never-written"), nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := store.Query("o/r", 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d records from an empty domain", len(got))
		}
	})
}
