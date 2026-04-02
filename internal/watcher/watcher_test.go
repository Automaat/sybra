package watcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	w := New("/tmp/test", func(string, any) {})
	if w == nil {
		t.Fatal("watcher is nil")
	}
	if w.dir != "/tmp/test" {
		t.Errorf("dir = %q, want %q", w.dir, "/tmp/test")
	}
}

func TestStartInvalidDir(t *testing.T) {
	w := New("/nonexistent/path/that/does/not/exist", func(string, any) {})
	err := w.Start(t.Context())
	if err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}

func TestStartAndEmitCreate(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var events []string

	emit := func(event string, _ any) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}

	w := New(dir, emit)
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mdPath := filepath.Join(dir, "test-task.md")
	if err := os.WriteFile(mdPath, []byte("# Task"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForAnyEvent := func() bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(events)
			mu.Unlock()
			if n > 0 {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	if !waitForAnyEvent() {
		t.Error("expected at least one event after creating .md file")
	}

	mu.Lock()
	hasCreate := slices.Contains(events, "task:created") || slices.Contains(events, "task:updated")
	mu.Unlock()

	if !hasCreate {
		t.Error("expected task:created or task:updated event")
	}
}

func TestStartAndEmitDelete(t *testing.T) {
	dir := t.TempDir()

	// Pre-create the file before starting watcher
	mdPath := filepath.Join(dir, "to-delete.md")
	if err := os.WriteFile(mdPath, []byte("# Delete me"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var events []string

	emit := func(event string, _ any) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}

	w := New(dir, emit)
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(mdPath); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := slices.Contains(events, "task:deleted")
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("expected task:deleted event")
}

func TestNonMarkdownIgnored(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var emitCount int

	emit := func(string, any) {
		mu.Lock()
		emitCount++
		mu.Unlock()
	}

	w := New(dir, emit)
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	count := emitCount
	mu.Unlock()

	if count != 0 {
		t.Errorf("emitted %d events for non-md file, want 0", count)
	}
}

func TestContextCancellation(t *testing.T) {
	dir := t.TempDir()

	// Use an already-expired deadline to test that the loop exits cleanly
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(200*time.Millisecond))
	defer cancel()

	w := New(dir, func(string, any) {})
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for deadline to expire and loop to exit
	time.Sleep(400 * time.Millisecond)
}

func TestDebounce(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var createCount int

	emit := func(event string, _ any) {
		if event == "task:created" {
			mu.Lock()
			createCount++
			mu.Unlock()
		}
	}

	w := New(dir, emit)
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	mdPath := filepath.Join(dir, "rapid.md")
	for range 5 {
		if err := os.WriteFile(mdPath, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	count := createCount
	mu.Unlock()

	if count >= 5 {
		t.Errorf("debounce failed: got %d events, expected fewer than 5", count)
	}
}
