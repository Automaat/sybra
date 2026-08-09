package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func eventsFixture() []Event {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return []Event{
		{Timestamp: base, Type: "agent.started", TaskID: "task-a", AgentID: "ag1"},
		{Timestamp: base.Add(time.Hour), Type: "agent.done", TaskID: "task-a", AgentID: "ag1"},
		{Timestamp: base.Add(2 * time.Hour), Type: "agent.started", TaskID: "task-b", AgentID: "ag2"},
	}
}

// TestSQLStore_ReadMatchesTheFileTrail is the issue's "reports show the same
// figures": every filter shape a caller uses must return what the files did.
func TestSQLStore_ReadMatchesTheFileTrail(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		files, err := NewLogger(dir)
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		t.Cleanup(func() { _ = files.Close() })
		sqlStore, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, e := range eventsFixture() {
			if err := files.Log(e); err != nil {
				t.Fatalf("file log: %v", err)
			}
			if err := sqlStore.Log(e); err != nil {
				t.Fatalf("sql log: %v", err)
			}
		}

		base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		for _, q := range []Query{
			{Since: base.Add(-time.Hour), Until: base.Add(6 * time.Hour)},
			{Since: base.Add(-time.Hour), Until: base.Add(6 * time.Hour), TaskID: "task-a"},
			{Since: base.Add(-time.Hour), Until: base.Add(6 * time.Hour), Type: "agent.started"},
			// A prefix, which is what the contract is: the file reader matches
			// with HasPrefix and the statistics backfill asks for exactly this.
			// Exact-match equality returns none of them, and "agent.started" on
			// its own is the one shape where both agree.
			{Since: base.Add(-time.Hour), Until: base.Add(6 * time.Hour), Type: "agent."},
			{Since: base.Add(-time.Hour), Until: base.Add(6 * time.Hour), Type: "agent"},
			{Since: base.Add(90 * time.Minute), Until: base.Add(6 * time.Hour)},
		} {
			fromFiles, err := files.Read(q)
			if err != nil {
				t.Fatalf("file read %+v: %v", q, err)
			}
			fromSQL, err := sqlStore.Read(q)
			if err != nil {
				t.Fatalf("sql read %+v: %v", q, err)
			}
			if len(fromSQL) != len(fromFiles) {
				t.Fatalf("query %+v: sql returned %d events, files returned %d", q, len(fromSQL), len(fromFiles))
			}
			for i := range fromFiles {
				if fromSQL[i].Type != fromFiles[i].Type || fromSQL[i].TaskID != fromFiles[i].TaskID ||
					!fromSQL[i].Timestamp.Equal(fromFiles[i].Timestamp) {
					t.Fatalf("query %+v position %d: sql %+v, files %+v", q, i, fromSQL[i], fromFiles[i])
				}
			}
		}
	})
}

// TestSQLStore_LoggingIsIdempotentPerEvent pins the property the resumable
// import leans on: a batch retried after a crash must not duplicate its rows.
func TestSQLStore_LoggingIsIdempotentPerEvent(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		e := eventsFixture()[0]
		for range 3 {
			if err := store.Log(e); err != nil {
				t.Fatalf("log: %v", err)
			}
		}
		got, err := store.Read(Query{Since: e.Timestamp.Add(-time.Hour), Until: e.Timestamp.Add(time.Hour)})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("logging one event three times produced %d rows", len(got))
		}
	})
}

// TestSQLStore_CleanupHonoursRetention is the issue's "retention still removes
// data past its configured age".
func TestSQLStore_CleanupHonoursRetention(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		now := time.Now().UTC()
		old := Event{Timestamp: now.AddDate(0, 0, -40), Type: "old", TaskID: "t"}
		fresh := Event{Timestamp: now.AddDate(0, 0, -1), Type: "fresh", TaskID: "t"}
		for _, e := range []Event{old, fresh} {
			if err := store.Log(e); err != nil {
				t.Fatalf("log: %v", err)
			}
		}
		if err := store.Cleanup(30); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		window := Query{Since: now.AddDate(0, 0, -365), Until: now.Add(time.Hour)}
		got, err := store.Read(window)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 || got[0].Type != "fresh" {
			t.Fatalf("after cleanup got %d events, want only the fresh one", len(got))
		}

		// Zero keeps everything, as the file trail does.
		if err := store.Cleanup(0); err != nil {
			t.Fatalf("cleanup(0): %v", err)
		}
		got, err = store.Read(window)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("cleanup(0) removed events; got %d", len(got))
		}
	})
}

// TestImport_ResumesWithoutDuplicating is the issue's "existing logs import
// once, and a partial import resumes without duplicating rows".
func TestImport_ResumesWithoutDuplicating(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		// Three day-files, so a partial import has somewhere to stop.
		for i, day := range []string{"2026-05-01", "2026-05-02", "2026-05-03"} {
			e := eventsFixture()[i]
			line, err := json.Marshal(e)
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
		got, err := store.Read(Query{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("after two imports got %d events, want 3", len(got))
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
		got, err := store.Read(Query{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d events from an empty domain", len(got))
		}
	})
}
