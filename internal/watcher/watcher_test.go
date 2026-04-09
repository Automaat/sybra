package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ev "github.com/Automaat/synapse/internal/events"
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
		if event != ev.TaskCreated && event != ev.TaskUpdated {
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
			if event == ev.TaskDeleted {
				return
			}
		case <-timeout:
			t.Error("expected " + ev.TaskDeleted + " event")
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
		if event == ev.TaskCreated {
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

// TestDebounceDoesNotDropTrailingWrite demonstrates that the watcher's
// leading-edge debounce silently drops the FINAL write in a burst when it
// arrives within the debounce window after an emitted event. The frontend
// is then never told to re-read the file and is left with stale content
// from before the trailing write.
//
// Sequence:
//
//	t=0     write v1            -> debounce empty -> emit, debounce[name]=t0
//	t=Δms   first event consumed
//	t=tw    write v2 (final)    -> debounce[name]=t0 set, tw-t0 < 200ms -> DROPPED
//	(no further events for this file)
//
// Expected behaviour: at least one event must arrive AFTER the trailing
// write so consumers can re-read and observe v2. A trailing-edge debounce
// (or "emit latest after quiet period") would satisfy this.
//
// Fix: instead of dropping events inside the window, schedule a deferred
// emission of the latest event after the window expires.
func TestDebounceDoesNotDropTrailingWrite(t *testing.T) {
	dir := t.TempDir()

	var (
		mu     sync.Mutex
		events []time.Time
	)
	firstSeen := make(chan struct{}, 1)
	emit := func(event string, _ any) {
		if event != ev.TaskCreated && event != ev.TaskUpdated {
			return
		}
		mu.Lock()
		events = append(events, time.Now())
		mu.Unlock()
		select {
		case firstSeen <- struct{}{}:
		default:
		}
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	mdPath := filepath.Join(dir, "rapid.md")

	// First write — fills the debounce slot for this file.
	if err := os.WriteFile(mdPath, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait until the first event has been observed so the debounce slot
	// is definitely populated. Without this synchronization, the test is
	// non-deterministic on slow systems.
	select {
	case <-firstSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("never observed an event for the first write")
	}

	// Tiny gap, well inside the 200ms debounce window.
	time.Sleep(50 * time.Millisecond)

	// Trailing write — this is the one consumers must be told about.
	secondWriteAt := time.Now()
	if err := os.WriteFile(mdPath, []byte("v2-final-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Allow plenty of time for any deferred emission. 500ms is well past
	// the 200ms debounce window so a correctly-implemented trailing-edge
	// debounce would have fired by now.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	var afterSecond int
	for _, ts := range events {
		if ts.After(secondWriteAt) {
			afterSecond++
		}
	}
	if afterSecond == 0 {
		t.Errorf("expected at least one event after the trailing write at %v; got %d events total at %v — the final write was silently dropped", secondWriteAt, len(events), events)
	}
}
