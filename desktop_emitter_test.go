//go:build darwin

package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// newTestEmitter builds an emitter without starting the background ticker, with
// an injectable clock and gate so flush() can be driven deterministically.
// emitted() returns a snapshot of everything emitted so far.
func newTestEmitter() (e *desktopEmitter, emitted func() []desktopEvent) {
	var mu sync.Mutex
	var got []desktopEvent
	e = &desktopEmitter{
		logger:        slog.New(slog.DiscardHandler),
		queue:         make(chan desktopEvent, desktopEmitQueueSize),
		interval:      desktopEmitInterval,
		maxGoroutines: desktopEmitMaxGoroutines,
		batchSize:     desktopEmitBatchSize,
		stallAfter:    desktopEmitStallAfter,
		pending:       map[string]desktopEvent{},
		now:           func() time.Time { return time.Unix(0, 0) },
		gated:         func() bool { return false },
	}
	e.emit = func(name string, data any) {
		mu.Lock()
		got = append(got, desktopEvent{name: name, data: data})
		mu.Unlock()
	}
	return e, func() []desktopEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]desktopEvent, len(got))
		copy(out, got)
		return out
	}
}

func TestDesktopEmitterCoalescesWhenQueueFull(t *testing.T) {
	e, emitted := newTestEmitter()
	e.queue = make(chan desktopEvent, 1)

	e.Emit("agent:output:1", "first")
	e.Emit("agent:output:1", "second") // queue full → coalesced, latest wins
	e.Emit("task:updated", "task-a")   // queue full → coalesced

	e.flush()

	got := emitted()
	if len(got) != 3 {
		t.Fatalf("expected 3 emitted events, got %d (%#v)", len(got), got)
	}
	if got[0].data != "first" {
		t.Fatalf("expected queued event to emit first, got %v", got[0].data)
	}
	if got[1].data != "second" && got[2].data != "second" {
		t.Fatalf("expected latest coalesced agent output to emit, got %#v", got)
	}
}

func TestDesktopEmitterPausesWhenGated(t *testing.T) {
	e, emitted := newTestEmitter()
	e.gated = func() bool { return true }
	e.stallAfter = time.Hour

	e.Emit("task:updated", "task-a")
	e.flush()

	if got := emitted(); len(got) != 0 {
		t.Fatalf("expected emit to pause while gated, emitted %#v", got)
	}
	if got := len(e.queue); got != 1 {
		t.Fatalf("expected queued event to remain queued, got %d", got)
	}
}

func TestDesktopEmitterDrainsBatchPerTick(t *testing.T) {
	e, emitted := newTestEmitter()
	total := desktopEmitBatchSize + 5
	for i := range total {
		e.Emit("e", i)
	}

	e.flush()

	if got := len(emitted()); got != desktopEmitBatchSize {
		t.Fatalf("first tick drained %d, want batch size %d", got, desktopEmitBatchSize)
	}
}

func TestDesktopEmitterStallEscalatesOnce(t *testing.T) {
	e, _ := newTestEmitter()
	e.gated = func() bool { return true }
	now := time.Unix(0, 0)
	e.now = func() time.Time { return now }

	var stalls []time.Duration
	e.onStall = func(d time.Duration) { stalls = append(stalls, d) }

	e.flush() // t=0: episode begins, under threshold
	if len(stalls) != 0 {
		t.Fatalf("alerted before stallAfter: %v", stalls)
	}

	now = now.Add(e.stallAfter + time.Second)
	e.flush() // past threshold → one alert
	e.flush() // still gated → must not re-alert
	now = now.Add(time.Minute)
	e.flush()

	if len(stalls) != 1 {
		t.Fatalf("onStall fired %d times, want exactly 1", len(stalls))
	}
	if stalls[0] < e.stallAfter {
		t.Fatalf("stall duration %s < stallAfter %s", stalls[0], e.stallAfter)
	}
}

func TestDesktopEmitterRecoveryAfterStall(t *testing.T) {
	e, _ := newTestEmitter()
	gated := true
	e.gated = func() bool { return gated }
	now := time.Unix(0, 0)
	e.now = func() time.Time { return now }

	var recovered []time.Duration
	e.onRecovered = func(d time.Duration) { recovered = append(recovered, d) }

	e.flush()                                 // begin episode
	now = now.Add(e.stallAfter + time.Second) // cross threshold
	e.flush()                                 // escalates

	gated = false
	e.flush() // recovers
	e.flush() // already recovered → no second announce

	if len(recovered) != 1 {
		t.Fatalf("onRecovered fired %d times, want exactly 1", len(recovered))
	}
}

func TestDesktopEmitterTransientPauseNoAlert(t *testing.T) {
	e, _ := newTestEmitter()
	gated := true
	e.gated = func() bool { return gated }
	now := time.Unix(0, 0)
	e.now = func() time.Time { return now }

	var stalls, recovered int
	e.onStall = func(time.Duration) { stalls++ }
	e.onRecovered = func(time.Duration) { recovered++ }

	e.flush() // gated briefly
	now = now.Add(time.Second)
	gated = false
	e.flush() // clears well before stallAfter

	if stalls != 0 || recovered != 0 {
		t.Fatalf("transient pause alerted (stalls=%d recovered=%d), want 0/0", stalls, recovered)
	}
}

func TestNewDesktopEmitterStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := newDesktopEmitter(ctx, slog.Default(), func(string, any) {})
	cancel()

	select {
	case e.queue <- desktopEvent{name: "task:updated"}:
	case <-time.After(time.Second):
		t.Fatal("emitter queue blocked after context cancel")
	}
}
