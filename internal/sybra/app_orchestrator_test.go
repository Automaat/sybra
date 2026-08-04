package sybra

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartWorktreeCleanupRunsAsyncAndSingleFlights(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	a := &App{worktreeCleanupFn: func(context.Context) {
		calls.Add(1)
		close(started)
		<-release
	}}

	a.startWorktreeCleanup(t.Context())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start asynchronously")
	}
	a.startWorktreeCleanup(t.Context())
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent cleanup calls = %d, want 1", got)
	}

	close(release)
	deadline := time.After(time.Second)
	for a.maintenanceCleanupRunning.Load() {
		select {
		case <-deadline:
			t.Fatal("cleanup remained marked running after return")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
