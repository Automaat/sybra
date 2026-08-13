// Package backendconformance provides one observable lifecycle suite for
// execution backends whose concrete event and handle types live in different
// packages.
package backendconformance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type Event struct {
	Kind string
	Err  error
}

type Recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *Recorder) Emit(event Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *Recorder) Snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

type Fixture struct {
	Start        func() (string, error)
	InvalidStart func() error
	Stop         func(string) error
	Recover      func(string, func(Event)) error
	Inspect      func(string) error
	Release      func()
	Steer        func(string, string) error
	Approve      func(string, bool) error
	Steered      func() bool
	Approved     func() bool
}

type Factory func(*testing.T, func(Event)) Fixture

// Run checks the common externally observable contract. Backend-specific
// controls may be nil when the implementation intentionally does not support
// them; lifecycle, recovery, cancellation, and exactly-once completion are
// mandatory for every fixture.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ordered completion and recovery", func(t *testing.T) {
		initial := &Recorder{}
		fixture := factory(t, initial.Emit)
		handle, err := fixture.Start()
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if fixture.Steer != nil {
			if err := fixture.Steer(handle, "continue"); err != nil || fixture.Steered == nil || !fixture.Steered() {
				t.Fatalf("Steer err=%v observed=%v", err, fixture.Steered != nil && fixture.Steered())
			}
		}
		if fixture.Approve != nil {
			if err := fixture.Approve(handle, true); err != nil || fixture.Approved == nil || !fixture.Approved() {
				t.Fatalf("Approve err=%v observed=%v", err, fixture.Approved != nil && fixture.Approved())
			}
		}
		if err := fixture.Inspect(handle); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		recovered := &Recorder{}
		if err := fixture.Recover(handle, recovered.Emit); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		fixture.Release()
		if !poll(time.Second*3, func() bool {
			events := recovered.Snapshot()
			return len(events) >= 2 && events[len(events)-1].Kind == "completed"
		}) {
			t.Fatalf("recovered events = %+v", recovered.Snapshot())
		}
		if events := initial.Snapshot(); len(events) != 1 || events[0].Kind != "started" {
			t.Fatalf("initial events = %+v, want only started", events)
		}
		events := recovered.Snapshot()
		if len(events) != 2 || events[0].Kind != "output" || events[1].Kind != "completed" {
			t.Fatalf("recovered events = %+v, want output then exactly one completed", events)
		}
		if err := fixture.Recover("unknown-handle", recovered.Emit); err == nil {
			t.Fatal("Recover accepted an unknown handle")
		}
	})

	t.Run("start error emits nothing", func(t *testing.T) {
		sink := &Recorder{}
		fixture := factory(t, sink.Emit)
		if err := fixture.InvalidStart(); err == nil {
			t.Fatal("invalid Start succeeded")
		}
		if events := sink.Snapshot(); len(events) != 0 {
			t.Fatalf("events after Start error = %+v", events)
		}
		fixture.Release()
	})

	t.Run("stop is idempotent and cancels once", func(t *testing.T) {
		sink := &Recorder{}
		fixture := factory(t, sink.Emit)
		handle, err := fixture.Start()
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := fixture.Stop(handle); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := fixture.Stop(handle); err != nil {
			t.Fatalf("repeated Stop: %v", err)
		}
		if !poll(time.Second*3, func() bool {
			for _, event := range sink.Snapshot() {
				if event.Kind == "completed" && errors.Is(event.Err, context.Canceled) {
					return true
				}
			}
			return false
		}) {
			t.Fatalf("stop events = %+v", sink.Snapshot())
		}
		completed := 0
		for _, event := range sink.Snapshot() {
			if event.Kind == "completed" {
				completed++
			}
		}
		if completed != 1 {
			t.Fatalf("completed count = %d, want 1", completed)
		}
	})
}

func poll(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}
