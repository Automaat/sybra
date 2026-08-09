package agentqueue

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func queueFixture() []Item {
	return []Item{
		{TaskID: "pr-review", Role: "review", Priority: task.PriorityLow, Status: task.StatusTodo},
		{TaskID: "prompt-lab", Role: "implementation", Priority: task.PriorityUrgent, Status: task.StatusTodo},
		{TaskID: "pr-fix", Role: "implementation", Priority: task.PriorityHigh, Status: task.StatusTodo, Manual: true},
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestSQLStore_OrderingSurvivesARestart is the issue's "queue ordering after a
// restart matches the ordering before it".
//
// The order does not come from the store — the queue sorts by the persisted
// fields — so this compares what a restarted queue actually dispatches, not
// what the store happened to return.
func TestSQLStore_OrderingSurvivesARestart(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, discard())
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		q, err := New(t.TempDir(), Options{Store: store}, discard())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, it := range queueFixture() {
			if !q.Offer(it) {
				t.Fatalf("Offer(%s) refused", it.TaskID)
			}
		}
		before := ids(q.Snapshot())

		// A restart is a fresh queue over the same mirror.
		again, err := New(t.TempDir(), Options{Store: store}, discard())
		if err != nil {
			t.Fatalf("New(restart): %v", err)
		}
		after := ids(again.Snapshot())

		if len(after) != len(before) {
			t.Fatalf("restart holds %d items, want %d (%v vs %v)", len(after), len(before), after, before)
		}
		for i := range before {
			if before[i] != after[i] {
				t.Fatalf("position %d: before restart %q, after %q (%v vs %v)", i, before[i], after[i], before, after)
			}
		}
	})
}

func ids(items []Item) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, items[i].TaskID)
	}
	return out
}

// TestSQLStore_RemoveIsDurable pins that a popped item does not come back on
// the next start, which is what a queue mirror exists to get right.
func TestSQLStore_RemoveIsDurable(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d, discard())
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		q, err := New(t.TempDir(), Options{Store: store}, discard())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, it := range queueFixture() {
			q.Offer(it)
		}
		q.Remove("pr-fix")
		again, err := New(t.TempDir(), Options{Store: store}, discard())
		if err != nil {
			t.Fatalf("New(restart): %v", err)
		}
		for _, id := range ids(again.Snapshot()) {
			if id == "pr-fix" {
				t.Fatal("a removed item came back after restart")
			}
		}
	})
}

// TestImport_IsOnceOnlyAndLeavesTheFiles pins the upgrade guarantees.
func TestImport_IsOnceOnlyAndLeavesTheFiles(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		fileStore, err := newStore(dir)
		if err != nil {
			t.Fatalf("newStore: %v", err)
		}
		for _, it := range queueFixture() {
			if err := fileStore.put(it); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", nil); err != nil {
				t.Fatalf("import: %v", err)
			}
		}
		store, err := NewSQLStore(d, discard())
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		if got := store.load(discard()); len(got) != 3 {
			t.Fatalf("after two imports the mirror holds %d items, want 3", len(got))
		}
		for _, it := range queueFixture() {
			if _, err := os.Stat(filepath.Join(dir, it.TaskID+".yaml")); err != nil {
				t.Errorf("import removed %s: %v", it.TaskID, err)
			}
		}
	})
}
