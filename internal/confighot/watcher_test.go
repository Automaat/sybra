package confighot

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForCount(t *testing.T, count *atomic.Int64, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for onChange count %d, got %d", want, count.Load())
}

func TestWatcher_SingleWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	w := New(path, func() { calls.Add(1) }, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	if err := os.WriteFile(path, []byte("level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForCount(t, &calls, 1, 3*time.Second)
	if got := calls.Load(); got != 1 {
		t.Errorf("got %d calls, want 1", got)
	}
}

func TestWatcher_BurstCoalescesToOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	w := New(path, func() { calls.Add(1) }, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	// 5 rapid writes within debounce window
	for range 5 {
		if err := os.WriteFile(path, []byte("level: debug\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	waitForCount(t, &calls, 1, 3*time.Second)
	// Pause to ensure no extra calls arrive
	time.Sleep(700 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Errorf("burst produced %d calls, want 1", got)
	}
}

func TestWatcher_AtomicSave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	w := New(path, func() { calls.Add(1) }, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	// Simulate vim/editor atomic save: write to tmp, rename over target
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("level: warn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	waitForCount(t, &calls, 1, 3*time.Second)
}

func TestWatcher_DeleteIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Pre-create the file
	if err := os.WriteFile(path, []byte("level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	w := New(path, func() { calls.Add(1) }, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// Wait long enough to see if any spurious call comes through
	time.Sleep(700 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("delete triggered %d calls, want 0", got)
	}
}

func TestWatcher_SiblingFileIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	w := New(path, func() { calls.Add(1) }, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	// Write a sibling file — should not trigger onChange
	sibling := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(sibling, []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(700 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("sibling write triggered %d calls, want 0", got)
	}
}

func TestWatcher_ContextCancelCleans(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	ctx, cancel := context.WithCancel(t.Context())

	w := New(path, func() {}, discardLogger())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	cancel()
	select {
	case <-w.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop after context cancel")
	}
}

func TestWatcher_NoGoroutineLeak(t *testing.T) {
	t.Parallel()

	// Allow any test-framework goroutines to settle before sampling baseline.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for range 10 {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		ctx, cancel := context.WithCancel(context.Background())

		w := New(path, func() {}, discardLogger())
		if err := w.Start(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		<-w.Ready()
		cancel()
		<-w.Done()
	}

	// Poll until goroutine count stabilises or timeout.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		after = runtime.NumGoroutine()
		if after-before <= 3 {
			return
		}
	}
	t.Errorf("goroutine leak: before=%d after=%d delta=%d", before, after, after-before)
}
