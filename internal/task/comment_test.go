package task

import (
	"sync"
	"testing"
)

func TestCommentStore_ListEmpty(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())
	comments, err := s.List("task-abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("expected empty, got %d comments", len(comments))
	}
}

func TestCommentStore_AddAndList(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())

	c, err := s.Add("task-1", 5, "needs fixing")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c.ID == "" {
		t.Error("expected non-empty ID")
	}
	if c.Line != 5 {
		t.Errorf("got line %d, want 5", c.Line)
	}
	if c.Body != "needs fixing" {
		t.Errorf("got body %q, want %q", c.Body, "needs fixing")
	}
	if c.Resolved {
		t.Error("expected Resolved=false")
	}

	comments, err := s.List("task-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	if comments[0].ID != c.ID {
		t.Errorf("ID mismatch: got %q, want %q", comments[0].ID, c.ID)
	}
}

// TestCommentStore_ConcurrentAddsDontDropWrites exercises the List→append→
// write race directly: without a per-task lock around the whole critical
// section, two goroutines can both List the same (empty) baseline and each
// write back a single-comment slice, so the loser's Add is silently
// discarded. With the lock, every Add lands.
func TestCommentStore_ConcurrentAddsDontDropWrites(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			if _, err := s.Add("task-race", i, "comment"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	comments, err := s.List("task-race")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(comments) != n {
		t.Errorf("got %d comments, want %d (a racing Add dropped an update)", len(comments), n)
	}
}

func TestCommentStore_AddMultiple(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())

	ids := make(map[string]bool)
	for i := range 3 {
		c, err := s.Add("task-1", i+1, "comment")
		if err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		ids[c.ID] = true
	}

	if len(ids) != 3 {
		t.Errorf("expected 3 unique IDs, got %d", len(ids))
	}

	comments, err := s.List("task-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(comments) != 3 {
		t.Errorf("got %d comments, want 3", len(comments))
	}
}

func TestCommentStore_Resolve(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())

	c, err := s.Add("task-1", 1, "issue")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.Resolve("task-1", c.ID); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	comments, err := s.List("task-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(comments) == 0 {
		t.Fatal("expected at least 1 comment")
	}
	if !comments[0].Resolved {
		t.Error("expected Resolved=true")
	}
}

func TestCommentStore_Resolve_NotFound(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())
	if err := s.Resolve("task-1", "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent comment")
	}
}

func TestCommentStore_Delete(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())

	c1, _ := s.Add("task-1", 1, "first")
	c2, _ := s.Add("task-1", 2, "second")

	if err := s.Delete("task-1", c1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	comments, err := s.List("task-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("got %d comments, want 1", len(comments))
	}
	if comments[0].ID != c2.ID {
		t.Errorf("remaining comment ID mismatch: got %q, want %q", comments[0].ID, c2.ID)
	}
}

func TestCommentStore_Delete_NotFound(t *testing.T) {
	t.Parallel()
	s := NewCommentStore(t.TempDir())
	if err := s.Delete("task-1", "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent comment")
	}
}
