package taskdb

import (
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

// TestCommentStore_ListEmptyReturnsEmptySlice is the issue's "a task's
// comments are readable... through whichever backend is configured" for a
// task that was never commented on — no row exists yet in task_sidecars.
func TestCommentStore_ListEmptyReturnsEmptySlice(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store := NewCommentStore(d)
		got, err := store.List("no-such-task")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("comments = %v, want empty", got)
		}
	})
}

func TestCommentStore_AddListResolveDelete(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store := NewCommentStore(d)

		c1, err := store.Add("task-1", 10, "first comment")
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if c1.ID == "" {
			t.Fatal("Add did not mint an id")
		}
		if c1.Resolved {
			t.Fatal("Add produced a resolved comment")
		}

		c2, err := store.Add("task-1", 20, "second comment")
		if err != nil {
			t.Fatalf("Add: %v", err)
		}

		got, err := store.List("task-1")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("comments = %+v, want 2", got)
		}

		if err := store.Resolve("task-1", c1.ID); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		got, err = store.List("task-1")
		if err != nil {
			t.Fatalf("List after resolve: %v", err)
		}
		for _, c := range got {
			if c.ID == c1.ID && !c.Resolved {
				t.Fatal("c1 not marked resolved")
			}
			if c.ID == c2.ID && c.Resolved {
				t.Fatal("c2 unexpectedly resolved")
			}
		}

		if err := store.Delete("task-1", c1.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err = store.List("task-1")
		if err != nil {
			t.Fatalf("List after delete: %v", err)
		}
		if len(got) != 1 || got[0].ID != c2.ID {
			t.Fatalf("comments after delete = %+v, want only c2", got)
		}
	})
}

func TestCommentStore_ResolveAll(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store := NewCommentStore(d)
		if _, err := store.Add("task-1", 1, "a"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := store.Add("task-1", 2, "b"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := store.ResolveAll("task-1"); err != nil {
			t.Fatalf("ResolveAll: %v", err)
		}
		got, err := store.List("task-1")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, c := range got {
			if !c.Resolved {
				t.Fatalf("comment %+v not resolved after ResolveAll", c)
			}
		}
	})
}

func TestCommentStore_ResolveMissingCommentErrors(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store := NewCommentStore(d)
		if _, err := store.Add("task-1", 1, "a"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := store.Resolve("task-1", "nonexistent"); err == nil {
			t.Fatal("expected an error resolving a missing comment")
		}
		if err := store.Delete("task-1", "nonexistent"); err == nil {
			t.Fatal("expected an error deleting a missing comment")
		}
	})
}

// TestCommentStore_ConcurrentAddOnFreshTaskLosesNoComment proves the
// ensureCommentsRow-then-lock sequence in mutate actually serializes two
// concurrent first-comment writers for a task with no existing
// task_sidecars row. Locking a row that does not exist yet locks nothing on
// postgres, so without ensureCommentsRow both writers would read "no
// comments", and whichever commits last would silently overwrite the
// other's comment instead of losing to a conflict or landing both.
func TestCommentStore_ConcurrentAddOnFreshTaskLosesNoComment(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store := NewCommentStore(d)

		const n = 8
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := store.Add("fresh-task", i, "concurrent comment")
				errs[i] = err
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("Add[%d]: %v", i, err)
			}
		}

		got, err := store.List("fresh-task")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != n {
			t.Fatalf("comments = %d, want %d — a concurrent writer lost its comment", len(got), n)
		}
	})
}
