package stats

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func runsFixture(now time.Time) []RunRecord {
	return []RunRecord{
		{ID: "run-1", TaskID: "task-a", ProjectID: "o/r", Mode: "headless", Role: "implementation",
			Provider: providerid.Claude, Model: "sonnet", CostUSD: 1.5, DurationS: 10, Timestamp: now.Add(-2 * time.Hour)},
		{ID: "run-2", TaskID: "task-a", ProjectID: "o/r", Mode: "headless", Role: "review",
			Provider: providerid.Codex, Model: "gpt", CostUSD: 0.5, DurationS: 5, Timestamp: now.Add(-time.Hour)},
		{ID: "run-3", TaskID: "task-b", ProjectID: "o/r", Mode: "headless", Role: "implementation",
			Provider: providerid.Claude, Model: "sonnet", CostUSD: 2, DurationS: 20, Timestamp: now.Add(-40 * 24 * time.Hour)},
	}
}

// TestSQLStore_ReportsMatchTheFileStore is the issue's "reports built from this
// data show the same figures as before the move".
func TestSQLStore_ReportsMatchTheFileStore(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		fileStore, err := NewStore(filepath.Join(t.TempDir(), "stats.ndjson"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		sqlStore, err := NewSQLStore(d, nil)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, r := range runsFixture(now) {
			if err := fileStore.Record(r); err != nil {
				t.Fatalf("file record: %v", err)
			}
			if err := sqlStore.Record(r); err != nil {
				t.Fatalf("sql record: %v", err)
			}
		}

		want := fileStore.QueryAt(now)
		got := sqlStore.QueryAt(now)
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal want: %v", err)
		}
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal got: %v", err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("report differs between backends:\nfiles: %s\nsql:   %s", wantJSON, gotJSON)
		}
		if sqlStore.Len() != fileStore.Len() {
			t.Errorf("Len = %d, want %d", sqlStore.Len(), fileStore.Len())
		}
	})
}

// TestSQLStore_AllForTaskReadsOnlyThatTask is the issue's "a query filtered by
// time or task reads only matching rows".
func TestSQLStore_AllForTaskReadsOnlyThatTask(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		store, err := NewSQLStore(d, nil)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, r := range runsFixture(now) {
			if err := store.Record(r); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
		got := store.AllForTask("task-a")
		if len(got) != 2 {
			t.Fatalf("AllForTask returned %d runs, want 2", len(got))
		}
		for _, r := range got {
			if r.TaskID != "task-a" {
				t.Fatalf("AllForTask returned a run for %q", r.TaskID)
			}
		}
		since := store.Since(now.Add(-90 * time.Minute))
		if len(since) != 1 || since[0].ID != "run-2" {
			t.Fatalf("Since returned %d runs, want only run-2", len(since))
		}
	})
}

// TestImport_ResumesWithoutDuplicating pins the resumable-import outcome over
// more records than one batch holds.
func TestImport_ResumesWithoutDuplicating(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		path := filepath.Join(t.TempDir(), "stats.ndjson")
		fileStore, err := NewStore(path)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		const total = importBatchSize + 25
		for i := range total {
			if err := fileStore.Record(RunRecord{
				ID: "run-" + strconv.Itoa(i), TaskID: "task-a", Mode: "headless",
				Role: "implementation", CostUSD: 1, Timestamp: now.Add(-time.Duration(i) * time.Minute),
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		for range 2 {
			if err := Import(t.Context(), d, path, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLStore(d, nil)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if n := store.Len(); n != total {
			t.Fatalf("after two imports the database holds %d runs, want %d", n, total)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if !bytes.Equal(after, before) {
			t.Error("import modified the original history file")
		}
	})
}

func TestImport_EmptyDomainReadsBackEmpty(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		if err := Import(t.Context(), d, filepath.Join(t.TempDir(), "never-written.ndjson"), "home-a", nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		store, err := NewSQLStore(d, nil)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if n := store.Len(); n != 0 {
			t.Fatalf("got %d runs from an empty domain", n)
		}
	})
}
