package toolledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func recordsFixture() []Record {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return []Record{
		{Timestamp: base, AgentID: "ag1", TaskID: "task-a", Tool: "Read", ToolUseID: "u1"},
		{Timestamp: base.Add(time.Hour), AgentID: "ag1", TaskID: "task-a", Tool: "Bash", ToolUseID: "u2", Decision: "allow"},
		{Timestamp: base.Add(2 * time.Hour), AgentID: "ag2", TaskID: "task-b", Tool: "Edit", ToolUseID: "u3"},
	}
}

// TestSQLStore_LogIsIdempotentAndFiltersByTime covers both properties the
// import leans on: a retried write does not duplicate, and a window read
// touches only the window.
func TestSQLStore_LogIsIdempotentAndFiltersByTime(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, r := range recordsFixture() {
			for range 2 {
				if err := store.Log(r); err != nil {
					t.Fatalf("log: %v", err)
				}
			}
		}
		all, err := store.Read(time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(all) != 3 {
			t.Fatalf("logging each record twice produced %d rows, want 3", len(all))
		}

		base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		window, err := store.Read(base.Add(30*time.Minute), base.Add(90*time.Minute))
		if err != nil {
			t.Fatalf("windowed read: %v", err)
		}
		if len(window) != 1 || window[0].Tool != "Bash" {
			t.Fatalf("windowed read returned %d records, want only the Bash call", len(window))
		}
	})
}

// TestImport_ResumesWithoutDuplicating pins the issue's resumable-import
// outcome for this domain.
func TestImport_ResumesWithoutDuplicating(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		for i, day := range []string{"2026-05-01", "2026-05-02", "2026-05-03"} {
			line, err := json.Marshal(recordsFixture()[i])
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, day+".ndjson"), append(line, '\n'), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := store.Read(time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("after two imports got %d records, want 3", len(got))
		}
		for _, day := range []string{"2026-05-01", "2026-05-02", "2026-05-03"} {
			if _, err := os.Stat(filepath.Join(dir, day+".ndjson")); err != nil {
				t.Errorf("import removed %s: %v", day, err)
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
		got, err := store.Read(time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d records from an empty domain", len(got))
		}
	})
}
