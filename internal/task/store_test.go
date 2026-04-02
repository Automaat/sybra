package task

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("store is nil")
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("not a directory")
	}
}

func TestStoreCreate(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	task, err := store.Create("Test task", "Body content", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if task.ID == "" {
		t.Error("ID is empty")
	}
	if task.Title != "Test task" {
		t.Errorf("Title = %q, want %q", task.Title, "Test task")
	}
	if task.Body != "Body content" {
		t.Errorf("Body = %q, want %q", task.Body, "Body content")
	}
	if task.AgentMode != "headless" {
		t.Errorf("AgentMode = %q, want %q", task.AgentMode, "headless")
	}
	if task.Status != StatusTodo {
		t.Errorf("Status = %q, want %q", task.Status, StatusTodo)
	}
	if task.FilePath == "" {
		t.Error("FilePath is empty")
	}

	if _, err := os.Stat(task.FilePath); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestStoreListEmpty(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("expected empty list, got %d", len(tasks))
	}
}

func TestStoreListMultiple(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, title := range []string{"Task A", "Task B", "Task C"} {
		if _, err := store.Create(title, "", "headless"); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Errorf("got %d tasks, want 3", len(tasks))
	}
}

func TestStoreListIgnoresNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create("Real task", "", "headless"); err != nil {
		t.Fatal(err)
	}
	// Write a non-markdown file
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a task"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Errorf("got %d tasks, want 1", len(tasks))
	}
}

func TestStoreGet(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Find me", "body", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
	if got.Title != "Find me" {
		t.Errorf("Title = %q, want %q", got.Title, "Find me")
	}
}

func TestStoreGetNotFound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestStoreUpdate(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Original", "original body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.Update(created.ID, map[string]any{
		"title":  "Updated",
		"status": "done",
		"body":   "new body",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if updated.Title != "Updated" {
		t.Errorf("Title = %q, want %q", updated.Title, "Updated")
	}
	if updated.Status != StatusDone {
		t.Errorf("Status = %q, want %q", updated.Status, StatusDone)
	}
	if updated.Body != "new body" {
		t.Errorf("Body = %q, want %q", updated.Body, "new body")
	}

	// Verify persisted
	reloaded, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Title != "Updated" {
		t.Errorf("persisted Title = %q, want %q", reloaded.Title, "Updated")
	}
}

func TestStoreUpdateNotFound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Update("nonexistent", map[string]any{"title": "x"})
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}
