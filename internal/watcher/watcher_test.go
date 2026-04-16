package watcher

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	ev "github.com/Automaat/sybra/internal/events"
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

// TestAtomicRenameOverExistingEmitsUpdate covers editors like vim/nvim that
// save by writing to a temp file and renaming it over the target. The rename
// fires fsnotify.Remove for the victim inode, but the file still exists after
// the rename — the watcher must classify this as an update, not a delete.
//
// Sequence:
//
//	write   tasks/t.md     (v1)  -> Create
//	write   tasks/.t.md.sw (v2)  -> (ignored, not .md)
//	rename  .t.md.sw → t.md      -> Remove + Create for t.md; file exists
//
// Expected: final event for t.md must NOT be TaskDeleted.
func TestAtomicRenameOverExistingEmitsUpdate(t *testing.T) {
	dir := t.TempDir()

	var (
		mu   sync.Mutex
		seen []string
	)
	emit := func(event string, _ any) {
		if event != ev.TaskCreated && event != ev.TaskUpdated && event != ev.TaskDeleted {
			return
		}
		mu.Lock()
		seen = append(seen, event)
		mu.Unlock()
	}

	target := filepath.Join(dir, "t.md")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	// Stage v2 outside the watched dir to avoid spurious intermediate events,
	// then hardlink it into the dir under a sibling name and rename over target.
	// Using a non-.md staging name keeps the watcher from filtering on a
	// transient sibling.
	staging := filepath.Join(dir, "t.md.tmp")
	if err := os.WriteFile(staging, []byte("v2-atomic-final"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, target); err != nil {
		t.Fatal(err)
	}

	// Wait well past the 200ms debounce window for any emission to flush.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, e := range seen {
		if e == ev.TaskDeleted {
			t.Errorf("got %s event after atomic rename; file still exists — should have been update. events=%v", ev.TaskDeleted, seen)
		}
	}
	if len(seen) == 0 {
		t.Errorf("expected at least one create/update event after atomic rename; got none")
	}
}

// TestWatcherNoGoroutineLeakOnCancel verifies that starting and cancelling N
// watchers does not leak goroutines. The watcher loop owns an fsnotify
// watcher plus a timer; a regression that forgot to `fw.Close()` or failed
// to signal `done` would leak one goroutine per Start + Close cycle.
// Running 20 cycles catches regressions that leak a small fixed number,
// since the drift then exceeds normal runtime goroutine jitter.
func TestWatcherNoGoroutineLeakOnCancel(t *testing.T) {
	// Warm up the runtime so the initial goroutine count is stable.
	runtime.GC()
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)

	startCount := runtime.NumGoroutine()

	for range 20 {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		w := New(dir, func(string, any) {}, discardLogger())
		if err := w.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-w.Ready()
		cancel()
		select {
		case <-w.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher did not exit after cancel")
		}
	}

	// Let the runtime settle — goroutine exit is observed lazily.
	runtime.GC()
	runtime.Gosched()
	time.Sleep(100 * time.Millisecond)

	endCount := runtime.NumGoroutine()
	// Allow a small slack for unrelated runtime goroutines (GC workers, etc.)
	// that may come and go, but 20 leaked loop goroutines would blow past this.
	const slack = 5
	if endCount-startCount > slack {
		t.Errorf("goroutine leak: start=%d end=%d diff=%d (>%d slack)", startCount, endCount, endCount-startCount, slack)
	}
}

// TestDebounceIsolatesPerFile verifies that a burst on file A does not cause
// events for file B to be coalesced or dropped. Each filename owns its own
// debounce slot; a regression that shared the slot would silently swallow
// writes to the other file.
func TestDebounceIsolatesPerFile(t *testing.T) {
	dir := t.TempDir()

	var (
		mu     sync.Mutex
		counts = map[string]int{}
	)
	emit := func(event string, data any) {
		if event != ev.TaskCreated && event != ev.TaskUpdated {
			return
		}
		name, _ := data.(string)
		mu.Lock()
		counts[filepath.Base(name)]++
		mu.Unlock()
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitReady(t, w)

	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")

	// Interleave writes: A burst then B burst, both inside the debounce window.
	for range 3 {
		if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := os.WriteFile(b, []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Wait well past the debounce window.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if counts["a.md"] < 1 {
		t.Errorf("expected >=1 event for a.md, got %d (counts=%v)", counts["a.md"], counts)
	}
	if counts["b.md"] < 1 {
		t.Errorf("expected >=1 event for b.md, got %d (counts=%v)", counts["b.md"], counts)
	}
}
