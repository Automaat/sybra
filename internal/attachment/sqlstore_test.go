package attachment

import (
	"bytes"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestSQLStore_ServesContentNotAPath is the issue's "callers receive content
// rather than a path" and "a client on another machine can retrieve any
// attachment": the bytes come back from the row, with nothing on disk involved.
func TestSQLStore_ServesContentNotAPath(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		payload := []byte("attachment bytes")
		meta, err := store.Put("task-a", Attachment{
			ID: "att-1", FileName: "notes.txt", ContentType: "text/plain",
			CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		}, payload)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if meta.SizeBytes != int64(len(payload)) {
			t.Errorf("size = %d, want %d", meta.SizeBytes, len(payload))
		}
		if meta.Path != "" {
			t.Errorf("stored metadata kept a path %q; nothing on disk backs this record", meta.Path)
		}

		got, back, err := store.Content("task-a", "att-1")
		if err != nil {
			t.Fatalf("Content: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("content = %q, want %q", got, payload)
		}
		if back.FileName != "notes.txt" || back.ContentType != "text/plain" {
			t.Errorf("metadata round-tripped as %+v", back)
		}
	})
}

// TestSQLStore_RejectsAnOversizeUploadNamingTheLimit is the issue's "an upload
// larger than the configured limit is rejected with the limit named".
func TestSQLStore_RejectsAnOversizeUploadNamingTheLimit(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 8)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		_, err = store.Put("task-a", Attachment{ID: "att-big", FileName: "big.bin"}, bytes.Repeat([]byte("x"), 9))
		if err == nil {
			t.Fatal("an upload over the limit was accepted")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("8 bytes")) {
			t.Fatalf("refusal %q does not name the configured limit", err)
		}
	})
}

// TestSQLStore_ListsOldestFirstAndDeletes pins the ordering a task's attachment
// list depends on, and that deletion is durable.
func TestSQLStore_ListsOldestFirstAndDeletes(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		for _, a := range []Attachment{
			{ID: "att-second", FileName: "b", CreatedAt: base.Add(time.Hour)},
			{ID: "att-first", FileName: "a", CreatedAt: base},
		} {
			if _, err := store.Put("task-a", a, []byte("x")); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		list, err := store.List("task-a")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 || list[0].ID != "att-first" {
			t.Fatalf("List returned %v, want the oldest first", list)
		}

		if err := store.Delete("task-a", "att-first"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, _, err := store.Content("task-a", "att-first"); err == nil {
			t.Error("a deleted attachment still serves content")
		}

		if err := store.DeleteTask("task-a"); err != nil {
			t.Fatalf("DeleteTask: %v", err)
		}
		list, err = store.List("task-a")
		if err != nil {
			t.Fatalf("List after DeleteTask: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("DeleteTask left %d attachments", len(list))
		}
	})
}

// TestImport_MovesContentAndPreservesOrder is the issue's "existing files
// import once, and their metadata and ordering are preserved".
func TestImport_MovesContentAndPreservesOrder(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		root := t.TempDir()
		fileStore, err := NewStore(root, 0)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		for i, name := range []string{"second.txt", "first.txt"} {
			if _, err := fileStore.Import("task-a", Attachment{
				ID:          "att-" + name,
				FileName:    name,
				ContentType: "text/plain",
				CreatedAt:   base.Add(time.Duration(1-i) * time.Hour),
			}, []byte(name)); err != nil {
				t.Fatalf("seed %s: %v", name, err)
			}
		}

		for range 2 {
			if err := Import(t.Context(), d, root, []string{"task-a"}, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		list, err := store.List("task-a")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("after two imports the board holds %d attachments, want 2", len(list))
		}
		if list[0].FileName != "first.txt" {
			t.Errorf("import lost the ordering: %v", list)
		}
		got, _, err := store.Content("task-a", list[0].ID)
		if err != nil {
			t.Fatalf("Content: %v", err)
		}
		if !bytes.Equal(got, []byte("first.txt")) {
			t.Errorf("content = %q, want the file's bytes", got)
		}
	})
}
