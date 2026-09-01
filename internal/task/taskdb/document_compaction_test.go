package taskdb

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestSQLStoreCompactsOversizedLegacyDocuments(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, database *db.DB) {
		t.Helper()
		store, err := NewSQLStore(database)
		if err != nil {
			t.Fatal(err)
		}
		record := sqlTask("abc12345", "legacy oversized")
		for i := range 120 {
			record.AgentRuns = append(record.AgentRuns, task.AgentRun{
				AgentID: "agent", Prompt: strings.Repeat("p", 12000), Result: strings.Repeat("r", 12000), TurnCount: i,
			})
		}
		raw, err := task.MarshalStored(record)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) <= task.MaxStoredDocumentBytes {
			t.Fatalf("fixture is only %d bytes", len(raw))
		}
		if _, err := database.ExecContext(t.Context(), database.Rebind(`INSERT INTO tasks
			(id, status, project_id, title, created_at, updated_at, deleted_at, doc)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), record.ID, string(record.Status), record.ProjectID, record.Title,
			db.TimeValue(record.CreatedAt), db.TimeValue(record.UpdatedAt), int64(0), string(raw)); err != nil {
			t.Fatal(err)
		}

		count, err := store.CompactOversizedDocuments(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("compacted %d rows, want 1", count)
		}
		var doc string
		if err := database.QueryRowContext(t.Context(), database.Rebind(`SELECT doc FROM tasks WHERE id = ?`), record.ID).Scan(&doc); err != nil {
			t.Fatal(err)
		}
		if len(doc) > task.MaxStoredDocumentBytes {
			t.Fatalf("compacted document is %d bytes", len(doc))
		}
		got, _, err := store.Get(t.Context(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DocumentCompaction == nil || got.DocumentCompaction.LargestBytesSeen != len(raw) {
			t.Fatalf("receipt = %+v, original bytes = %d", got.DocumentCompaction, len(raw))
		}

		count, err = store.CompactOversizedDocuments(t.Context())
		if err != nil || count != 0 {
			t.Fatalf("second sweep = (%d, %v), want no-op", count, err)
		}
	})
}

func TestPersistenceReturnsCompactedTask(t *testing.T) {
	database := dbtest.SQLite(t)
	store, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPersistence(store)
	record := sqlTask("abc12345", "new oversized")
	record.AgentRuns = []task.AgentRun{{AgentID: "a1", Prompt: strings.Repeat("p", 10000)}}
	saved, err := p.PutBy(record, "operator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DocumentCompaction == nil || len(saved.AgentRuns[0].Prompt) > task.MaxStoredRunTextBytes {
		t.Fatalf("returned unbounded task: %+v", saved.DocumentCompaction)
	}
}

func TestRestoreCompactsOversizedDeletedDocument(t *testing.T) {
	database := dbtest.SQLite(t)
	store, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	record := sqlTask("abc12345", "deleted oversized")
	record.AgentRuns = []task.AgentRun{{AgentID: "a1", Prompt: strings.Repeat("p", task.MaxStoredDocumentBytes+1000)}}
	raw, err := task.MarshalStored(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), database.Rebind(`INSERT INTO tasks
		(id, status, project_id, title, created_at, updated_at, deleted_at, doc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), record.ID, string(record.Status), record.ProjectID, record.Title,
		db.TimeValue(record.CreatedAt), db.TimeValue(record.UpdatedAt), db.TimeValue(record.UpdatedAt), string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreBy(t.Context(), record.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	var doc string
	if err := database.QueryRowContext(t.Context(), database.Rebind(`SELECT doc FROM tasks WHERE id = ?`), record.ID).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) > task.MaxStoredDocumentBytes {
		t.Fatalf("restored document is %d bytes", len(doc))
	}
	got, _, err := store.Get(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocumentCompaction == nil {
		t.Fatal("restored task has no compaction receipt")
	}
}
