//go:build darwin

package main

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestDesktopEmitterCoalescesWhenQueueFull(t *testing.T) {
	var emitted []desktopEvent
	e := &desktopEmitter{
		emit: func(name string, data any) {
			emitted = append(emitted, desktopEvent{name: name, data: data})
		},
		logger:        slog.Default(),
		queue:         make(chan desktopEvent, 1),
		interval:      time.Hour,
		maxGoroutines: 1_000_000,
		pending:       map[string]desktopEvent{},
	}

	e.Emit("agent:output:1", "first")
	e.Emit("agent:output:1", "second")
	e.Emit("task:updated", "task-a")

	e.flushOne()
	e.flushOne()
	e.flushOne()

	if len(emitted) != 3 {
		t.Fatalf("expected 3 emitted events, got %d", len(emitted))
	}
	if emitted[0].data != "first" {
		t.Fatalf("expected queued event to emit first, got %v", emitted[0].data)
	}
	if emitted[1].data != "second" && emitted[2].data != "second" {
		t.Fatalf("expected latest coalesced agent output to emit, got %#v", emitted)
	}
}

func TestDesktopEmitterPausesWhenGoroutineLimitExceeded(t *testing.T) {
	called := false
	e := &desktopEmitter{
		emit: func(string, any) {
			called = true
		},
		logger:        slog.Default(),
		queue:         make(chan desktopEvent, 1),
		interval:      time.Hour,
		maxGoroutines: 0,
		pending:       map[string]desktopEvent{},
	}

	e.Emit("task:updated", "task-a")
	e.flushOne()

	if called {
		t.Fatal("expected emit to pause when goroutine limit is exceeded")
	}
	if got := len(e.queue); got != 1 {
		t.Fatalf("expected queued event to remain queued, got %d", got)
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
