package taskdb

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func sqlTask(id, title string) task.Task {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return task.Task{
		ID: id, Title: title, Status: task.StatusTodo, AgentMode: task.AgentModeHeadless,
		Body: "## Description\n\nwork\n", CreatedAt: now, UpdatedAt: now,
	}
}

// TestSQLStore_TaskAndSidecarsWriteTogether is the issue's "a task and its
// sidecars update together or not at all".
//
// As files this was several separate writes, so a crash between them left a
// task disagreeing with its own plan and nothing afterwards could tell which
// was current.
func TestSQLStore_TaskAndSidecarsWriteTogether(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		record := sqlTask("abc12345", "first")
		sidecars := []Sidecar{
			{Kind: SidecarPlan, Content: "# Plan\n"},
			{Kind: SidecarCodeReview, Content: "# Review\n"},
			{Kind: SidecarPlanDraft, Name: "a", Content: "draft a\n"},
			{Kind: SidecarPlanDraft, Name: "b", Content: "draft b\n"},
		}
		if err := store.Put(t.Context(), record, sidecars); err != nil {
			t.Fatalf("Put: %v", err)
		}

		got, gotSidecars, err := store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Title != "first" || got.Status != task.StatusTodo {
			t.Fatalf("task round-tripped as %+v", got)
		}
		if len(gotSidecars) != 4 {
			t.Fatalf("read %d sidecars, want 4", len(gotSidecars))
		}

		// A rewrite replaces the set: a sidecar dropped from the task is gone
		// with it, in the same transaction.
		onlyPlan := []Sidecar{{Kind: SidecarPlan, Content: "# Plan\n"}}
		if err := store.Put(t.Context(), record, onlyPlan); err != nil {
			t.Fatalf("second Put: %v", err)
		}
		_, gotSidecars, err = store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get after rewrite: %v", err)
		}
		if len(gotSidecars) != 1 || gotSidecars[0].Kind != SidecarPlan {
			t.Fatalf("rewrite left %+v, want only the plan", gotSidecars)
		}
	})
}

// TestSQLStore_DeletedTaskStaysRecoverable is the issue's "a deleted task stays
// recoverable until the retention window passes".
func TestSQLStore_DeletedTaskStaysRecoverable(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if err := store.Put(t.Context(), sqlTask("abc12345", "doomed"), nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := store.Delete(t.Context(), "abc12345"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, _, err := store.Get(t.Context(), "abc12345"); err == nil {
			t.Fatal("a deleted task still reads back from the board")
		}
		list, err := store.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("a deleted task still lists: %d", len(list))
		}

		if err := store.Restore(t.Context(), "abc12345"); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		back, _, err := store.Get(t.Context(), "abc12345")
		if err != nil {
			t.Fatalf("Get after restore: %v", err)
		}
		if back.Title != "doomed" {
			t.Errorf("restored task is %+v", back)
		}

		// Past retention it goes for good.
		if err := store.Delete(t.Context(), "abc12345"); err != nil {
			t.Fatalf("second Delete: %v", err)
		}
		if err := store.PurgeDeleted(t.Context(), -time.Hour+time.Hour); err != nil {
			t.Fatalf("PurgeDeleted(0): %v", err)
		}
		if err := store.Restore(t.Context(), "abc12345"); err != nil {
			t.Fatalf("Restore within retention: %v", err)
		}
		if _, _, err := store.Get(t.Context(), "abc12345"); err != nil {
			t.Fatalf("a zero retention purged a task that was still within it: %v", err)
		}
	})
}

// TestImport_ReportsUnparseableAndStillCompletes is the issue's "a file that
// fails to parse during import is reported and skipped, and the import still
// completes".
func TestImport_ReportsUnparseableAndStillCompletes(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		for _, id := range []string{"aaa11111", "bbb22222"} {
			data, err := task.MarshalStored(sqlTask(id, "task "+id))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".md"), data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		// A file no parser accepts, sitting between the two good ones.
		if err := os.WriteFile(filepath.Join(dir, "abb00000.md"), []byte("---\nnot: [valid\n"), 0o600); err != nil {
			t.Fatalf("write bad: %v", err)
		}
		// Sidecars belonging to the first task.
		for suffix, want := range map[string]string{".plan.md": "# Plan\n", ".code-review.md": "# Review\n"} {
			if err := os.WriteFile(filepath.Join(dir, "aaa11111"+suffix), []byte(want), 0o600); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
		}

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", logger); err != nil {
				t.Fatalf("import: %v", err)
			}
		}

		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		list, err := store.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("after two imports the board holds %d tasks, want the 2 that parse", len(list))
		}
		if !strings.Contains(buf.String(), "task.import.unparseable") {
			t.Fatalf("the unreadable task was skipped silently; log was %q", buf.String())
		}

		_, sidecars, err := store.Get(t.Context(), "aaa11111")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(sidecars) != 2 {
			t.Fatalf("imported %d sidecars for the first task, want 2", len(sidecars))
		}
		if _, err := os.Stat(filepath.Join(dir, "abb00000.md")); err != nil {
			t.Errorf("the unreadable file was removed: %v", err)
		}
	})
}
