package artifact

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestSQLStore_ServesContentNotAPath is the issue's "callers receive content
// rather than a path" and "a client on another machine can retrieve any
// artifact".
func TestSQLStore_ServesContentNotAPath(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		payload := []byte("# Code Review\n\nLooks good.\n")
		meta, err := store.Put("abc12345", Artifact{Kind: KindGeneric, Content: payload})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if meta.Size != int64(len(payload)) {
			t.Errorf("size = %d, want %d", meta.Size, len(payload))
		}
		got, back, err := store.Read("abc12345", meta.Name)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("content = %q, want %q", got, payload)
		}
		if back.Kind != KindGeneric {
			t.Errorf("kind = %q, want %q", back.Kind, KindGeneric)
		}
	})
}

// TestSQLStore_AppendIsAppendOnlyAcrossRestarts is the issue's "appending to a
// streamed artifact stays append-only and survives a restart mid-stream". A
// fresh store over the same database is what a restart looks like.
func TestSQLStore_AppendIsAppendOnlyAcrossRestarts(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		first, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for i := range 3 {
			if err := first.Append("abc12345", KindTrace, map[string]any{"n": i}); err != nil {
				t.Fatalf("Append %d: %v", i, err)
			}
		}

		restarted, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore(restart): %v", err)
		}
		for i := 3; i < 6; i++ {
			if err := restarted.Append("abc12345", KindTrace, map[string]any{"n": i}); err != nil {
				t.Fatalf("Append after restart %d: %v", i, err)
			}
		}

		content, _, err := restarted.Read("abc12345", KindTrace.defaultName())
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
		if len(lines) != 6 {
			t.Fatalf("stream holds %d lines, want 6: %q", len(lines), content)
		}
		for i, line := range lines {
			want := "\"n\":" + string(rune('0'+i))
			if !strings.Contains(line, want) {
				t.Fatalf("line %d is %q, want it to carry %s — appends must not reorder or replace", i, line, want)
			}
		}
	})
}

// TestSQLStore_RejectsOversizeNamingTheLimit pins that the refusal says what to
// change rather than only that the artifact was too big.
func TestSQLStore_RejectsOversizeNamingTheLimit(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 16)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		_, err = store.Put("abc12345", Artifact{Kind: KindGeneric, Content: bytes.Repeat([]byte("x"), 32)})
		if err == nil {
			t.Fatal("an artifact over the limit was accepted")
		}
		if !strings.Contains(err.Error(), "16 bytes") {
			t.Fatalf("refusal %q does not name the configured limit", err)
		}
	})
}

// TestSQLStore_ListsAndDeletesPerTask pins the listing order and that deleting
// a task clears its artifacts.
func TestSQLStore_ListsAndDeletesPerTask(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, name := range []string{"pr-review.md", "pr-fix.md", "branch-conflict.md"} {
			if _, err := store.Put("abc12345", Artifact{Name: name, Kind: KindGeneric, Content: []byte(name)}); err != nil {
				t.Fatalf("Put %s: %v", name, err)
			}
		}
		list, err := store.List("abc12345")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 3 || list[0].Name != "branch-conflict.md" {
			t.Fatalf("List returned %v, want name order", names(list))
		}

		ids, err := store.ListTaskIDs()
		if err != nil {
			t.Fatalf("ListTaskIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != "abc12345" {
			t.Fatalf("ListTaskIDs = %v", ids)
		}

		if err := store.Delete("abc12345"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		list, err = store.List("abc12345")
		if err != nil {
			t.Fatalf("List after delete: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("Delete left %d artifacts", len(list))
		}
	})
}

func names(list []Meta) []string {
	out := make([]string, 0, len(list))
	for i := range list {
		out = append(out, list[i].Name)
	}
	return out
}

// TestSQLStore_ConcurrentAppendsKeepEveryLine is the case a shared board exists for.
//
// Append is a read-modify-write. A plain transaction does not serialize it on postgres: two appends to a stream that does not exist yet both read no row, both insert, and the conflict clause resolves the collision by overwriting — so one agent's line vanishes with no error. Under a per-stream advisory lock the second append waits and reads the first one's row.
func TestSQLStore_ConcurrentAppendsKeepEveryLine(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, 0)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}

		const writers, perWriter = 4, 15
		var wg sync.WaitGroup
		for w := range writers {
			wg.Go(func() {
				for i := range perWriter {
					if err := store.Append("task-append", KindTrace, map[string]any{"w": w, "i": i}); err != nil {
						t.Errorf("append(%d,%d): %v", w, i, err)
						return
					}
				}
			})
		}
		wg.Wait()

		content, meta, err := store.Read("task-append", KindTrace.defaultName())
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		lines := bytes.Count(content, []byte("\n"))
		if want := writers * perWriter; lines != want {
			t.Fatalf("stream holds %d lines, want %d; concurrent appends dropped %d", lines, want, want-lines)
		}
		if meta.Size != int64(len(content)) {
			t.Errorf("recorded size %d does not match the %d bytes stored", meta.Size, len(content))
		}
	})
}
