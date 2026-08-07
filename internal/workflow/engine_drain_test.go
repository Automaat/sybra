package workflow

import (
	"context"
	"testing"
	"time"
)

// TestDrainContextFallsBackToEngineContext keeps the drain tier optional:
// embedders and tests that never bind one must still get an interruptible
// wait rather than a nil context.
func TestDrainContextFallsBackToEngineContext(t *testing.T) {
	t.Parallel()

	engineCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &Engine{ctx: engineCtx}

	if got := e.drainContext(); got != engineCtx {
		t.Fatalf("drainContext() = %v, want the engine context when no drain context is bound", got)
	}

	drainCtx, drainCancel := context.WithCancel(t.Context())
	defer drainCancel()
	e.SetDrainContext(drainCtx)
	if got := e.drainContext(); got != drainCtx {
		t.Fatalf("drainContext() = %v, want the bound drain context", got)
	}
}

// TestExecClassifyTask_BackoffAbandonsOnDrain is the call-site guard: it fails
// if execClassifyTask waits on the engine context instead of the drain
// context. The engine context stays live throughout, exactly as it does during
// App.Shutdown's drain phase, so a backoff bound to it would block until the
// real (multi-second) retry schedule elapsed rather than returning at once.
func TestExecClassifyTask_BackoffAbandonsOnDrain(t *testing.T) {
	// Not parallel: it swaps the package-level backoff schedule that the test
	// init() zeroes out, and must restore it for the other classify tests.
	restore := classifyTaskRetryBackoffs
	classifyTaskRetryBackoffs = []time.Duration{time.Hour, time.Hour, time.Hour}
	t.Cleanup(func() { classifyTaskRetryBackoffs = restore })

	// Signal the moment the first attempt fails and enters the backoff wait,
	// so the test can begin draining exactly then instead of guessing with a
	// fixed sleep.
	enteredBackoff := make(chan struct{}, 1)
	restoreWait := classifyTaskWait
	classifyTaskWait = func(ctx context.Context, d time.Duration) {
		select {
		case enteredBackoff <- struct{}{}:
		default:
		}
		restoreWait(ctx, d)
	}
	t.Cleanup(func() { classifyTaskWait = restoreWait })

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetTaskClassifier(&ctxCancelClassifier{})

	engineCtx, cancelEngine := context.WithCancel(t.Context())
	defer cancelEngine()
	engine.SetContext(engineCtx)
	drainCtx, beginDrain := context.WithCancel(t.Context())
	engine.SetDrainContext(drainCtx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = engine.execClassifyTask("t1", newClassifyTaskStep(), &Execution{})
	}()

	// Let the first attempt fail and enter the backoff, then begin draining.
	select {
	case <-enteredBackoff:
	case <-time.After(10 * time.Second):
		t.Fatal("execClassifyTask never entered the retry backoff")
	}
	beginDrain()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("execClassifyTask did not return once the app began draining; its retry backoff is bound to the engine context, which App.Shutdown cancels only after the drain it blocks")
	}
}

// TestClassifyRetryBackoffWakesOnDrain is the regression guard for the
// shutdown deadlock: the classify retry backoff has to abandon when the app
// begins draining, which happens strictly before the hard stop that cancels
// the engine context. Waiting on the engine context instead made the backoff
// outlive the drain — and since the drain waits for exactly this goroutine,
// it blocked on the cancellation it was itself delaying, burning App.Shutdown's
// full grace and then proceeding with the goroutine still live.
func TestClassifyRetryBackoffWakesOnDrain(t *testing.T) {
	t.Parallel()

	// Engine context stays live, mirroring the drain phase.
	engineCtx, cancelEngine := context.WithCancel(t.Context())
	defer cancelEngine()
	drainCtx, beginDrain := context.WithCancel(t.Context())
	e := &Engine{ctx: engineCtx, drainCtx: drainCtx}

	done := make(chan struct{})
	go func() {
		defer close(done)
		classifyTaskWait(e.drainContext(), time.Hour)
	}()

	select {
	case <-done:
		t.Fatal("backoff returned before the drain began")
	case <-time.After(50 * time.Millisecond):
	}

	beginDrain()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("classify retry backoff did not wake when the app began draining")
	}
}
