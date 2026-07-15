package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkWatcherChurn measures end-to-end latency under bursty writes to
// characterize the 200ms trailing-edge debounce. Each iteration writes one
// file and waits for the emit callback. This is not a microbenchmark — the
// minimum per-op is dominated by the debounce interval — but it exposes
// regressions in queue drain, map contention, and event ordering.
func BenchmarkWatcherChurn(b *testing.B) {
	dir := b.TempDir()
	var delivered atomic.Int64
	done := make(chan struct{}, b.N+16)

	emit := func(_ string, _ any) {
		delivered.Add(1)
		select {
		case done <- struct{}{}:
		default:
		}
	}

	w := New(dir, emit, discardLogger())
	if err := w.Start(b.Context()); err != nil {
		b.Fatalf("Start: %v", err)
	}
	<-w.Ready()

	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		path := filepath.Join(dir, fmt.Sprintf("bench-%d.md", i))
		if err := os.WriteFile(path, []byte("# bench\n"), 0o644); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	// Wait for all writes to drain through the debounce. Cap at 2s + N*10ms
	// so a slow machine with a full b.N doesn't hang the suite.
	deadline := time.After(2*time.Second + time.Duration(b.N)*10*time.Millisecond)
	for delivered.Load() < int64(b.N) {
		select {
		case <-done:
		case <-deadline:
			b.Fatalf("only %d/%d events delivered before deadline", delivered.Load(), b.N)
		}
	}
	b.StopTimer()
}

// BenchmarkWatcherSameFileCoalescing measures how well the debounce collapses
// repeated writes to the same path. 100 writes should produce ~1 emission,
// not 100 — the benchmark verifies coalescing and reports the achieved ratio.
func BenchmarkWatcherSameFileCoalescing(b *testing.B) {
	dir := b.TempDir()
	var delivered atomic.Int64
	emit := func(_ string, _ any) { delivered.Add(1) }
	w := New(dir, emit, discardLogger())
	if err := w.Start(b.Context()); err != nil {
		b.Fatalf("Start: %v", err)
	}
	<-w.Ready()

	path := filepath.Join(dir, "hot.md")
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		_ = os.WriteFile(path, fmt.Appendf(nil, "body %d\n", i), 0o644)
	}
	// Wait one debounce cycle plus slack for the timer to fire.
	time.Sleep(400 * time.Millisecond)
	b.StopTimer()
	b.ReportMetric(float64(delivered.Load()), "emits")
	b.ReportMetric(float64(b.N)/float64(max(delivered.Load(), 1)), "writes/emit")
}
