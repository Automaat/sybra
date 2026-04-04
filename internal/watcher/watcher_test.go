package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitReady(t *testing.T, w *Watcher) {
	t.Helper()
	select {
	case <-w.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher not ready in time")
	}
}

func TestNew(t *testing.T) {
	w := New("/tmp/test", func(string, any) {}, discardLogger())
	if w == nil {
		t.Fatal("watcher is nil")
	}
	if w.dir != "/tmp/test" {
		t.Errorf("dir = %q, want %q", w.dir, "/tmp/test")
	}
}

func TestStartInvalidDir(t *testing.T) {
	w := New("/nonexistent/path/that/does/not/exist", func(string, any) {}, discardLogger())
	err := w.Start(t.Context())
	if err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
}

func TestStartAndEmitCreate(t *testing.T) {
	dir := t.TempDir()

	got := make(chan string, 10)
	emit := func(event string, _ any) {
		select {
		case got <- event:
		default:
		}
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	mdPath := filepath.Join(dir, "test-task.md")
	if err := os.WriteFile(mdPath, []byte("# Task"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-got:
		if event != "task:created" && event != "task:updated" {
			t.Errorf("unexpected event %q, want task:created or task:updated", event)
		}
	case <-time.After(2 * time.Second):
		t.Error("expected at least one event after creating .md file")
	}
}

func TestStartAndEmitDelete(t *testing.T) {
	dir := t.TempDir()

	// Pre-create the file before starting watcher
	mdPath := filepath.Join(dir, "to-delete.md")
	if err := os.WriteFile(mdPath, []byte("# Delete me"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 10)
	emit := func(event string, _ any) {
		select {
		case got <- event:
		default:
		}
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	if err := os.Remove(mdPath); err != nil {
		t.Fatal(err)
	}

	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-got:
			if event == "task:deleted" {
				return
			}
		case <-timeout:
			t.Error("expected task:deleted event")
			return
		}
	}
}

func TestNonMarkdownIgnored(t *testing.T) {
	dir := t.TempDir()

	got := make(chan string, 1)
	emit := func(event string, _ any) {
		select {
		case got <- event:
		default:
		}
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait past debounce window; any event arriving is a failure.
	select {
	case event := <-got:
		t.Errorf("expected no events for non-md file, got %q", event)
	case <-time.After(400 * time.Millisecond):
		// no events, as expected
	}
}

func TestContextCancellation(t *testing.T) {
	dir := t.TempDir()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(200*time.Millisecond))
	defer cancel()

	w := New(dir, func(string, any) {}, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-w.Done():
		// goroutine exited cleanly after context cancellation
	case <-time.After(2 * time.Second):
		t.Error("watcher goroutine did not exit after context cancellation")
	}
}

func TestDebounce(t *testing.T) {
	dir := t.TempDir()

	got := make(chan string, 10)
	emit := func(event string, _ any) {
		if event == "task:created" {
			select {
			case got <- event:
			default:
			}
		}
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	mdPath := filepath.Join(dir, "rapid.md")
	for range 5 {
		if err := os.WriteFile(mdPath, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Drain events until debounce window expires with no new events.
	var count int
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case <-got:
			count++
		case <-timeout:
			if count >= 5 {
				t.Errorf("debounce failed: got %d events, expected fewer than 5", count)
			}
			return
		}
	}
}
