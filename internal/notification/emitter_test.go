package notification

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/events"
)

func nopEmit(_ string, _ any) {}

func TestSendBuffersNotification(t *testing.T) {
	e := New(nopEmit)
	e.SetDesktop(false)

	e.Send(LevelInfo, "title", "msg", "t1", "a1")

	list := e.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(list))
	}
	n := list[0]
	if n.Level != LevelInfo {
		t.Errorf("level: got %s, want info", n.Level)
	}
	if n.Title != "title" {
		t.Errorf("title: got %s, want title", n.Title)
	}
	if n.Message != "msg" {
		t.Errorf("message: got %s, want msg", n.Message)
	}
	if n.TaskID != "t1" {
		t.Errorf("taskID: got %s, want t1", n.TaskID)
	}
	if n.AgentID != "a1" {
		t.Errorf("agentID: got %s, want a1", n.AgentID)
	}
	if n.ID == "" {
		t.Error("ID should be non-empty")
	}
	if n.CreatedAt == "" {
		t.Error("CreatedAt should be non-empty")
	}
}

func TestSendEmitsEvent(t *testing.T) {
	type emitCall struct {
		event string
		data  any
	}
	var mu sync.Mutex
	var calls []emitCall

	emit := func(event string, data any) {
		mu.Lock()
		calls = append(calls, emitCall{event, data})
		mu.Unlock()
	}

	e := New(emit)
	e.SetDesktop(false)
	e.Send(LevelSuccess, "t", "m", "", "")

	mu.Lock()
	count := len(calls)
	var call emitCall
	if count > 0 {
		call = calls[0]
	}
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 emit, got %d", count)
	}
	if call.event != events.Notification {
		t.Errorf("event name: got %s, want %s", call.event, events.Notification)
	}
	n, ok := call.data.(Notification)
	if !ok {
		t.Fatalf("emitted value type %T, want Notification", call.data)
	}
	if n.Level != LevelSuccess {
		t.Errorf("emitted level: got %s, want success", n.Level)
	}
	if n.Title != "t" {
		t.Errorf("emitted title: got %s, want t", n.Title)
	}
}

func TestListNewestFirst(t *testing.T) {
	e := New(nopEmit)
	e.SetDesktop(false)

	e.Send(LevelInfo, "first", "", "", "")
	e.Send(LevelInfo, "second", "", "", "")
	e.Send(LevelInfo, "third", "", "", "")

	list := e.List()
	if len(list) != 3 {
		t.Fatalf("expected 3, got %d", len(list))
	}
	if list[0].Title != "third" {
		t.Errorf("newest first: got %s, want third", list[0].Title)
	}
	if list[2].Title != "first" {
		t.Errorf("oldest last: got %s, want first", list[2].Title)
	}
}

func TestBufferRingCapacity(t *testing.T) {
	e := New(nopEmit)
	e.SetDesktop(false)

	extra := 5
	for i := 0; i < ringCap+extra; i++ {
		e.Send(LevelInfo, fmt.Sprintf("n%d", i), "", "", "")
	}

	list := e.List()
	if len(list) != ringCap {
		t.Fatalf("expected %d notifications, got %d", ringCap, len(list))
	}
	// Newest should be the last sent.
	wantNewest := fmt.Sprintf("n%d", ringCap+extra-1)
	if list[0].Title != wantNewest {
		t.Errorf("newest: got %s, want %s", list[0].Title, wantNewest)
	}
	// Oldest should be n5 (first `extra` evicted).
	wantOldest := fmt.Sprintf("n%d", extra)
	if list[ringCap-1].Title != wantOldest {
		t.Errorf("oldest: got %s, want %s", list[ringCap-1].Title, wantOldest)
	}
}

func TestConcurrentSend(t *testing.T) {
	e := New(nopEmit)
	e.SetDesktop(false)

	const goroutines = 20
	const perGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				e.Send(LevelInfo, "t", "m", "", "")
			}
		}()
	}
	wg.Wait()

	total := goroutines * perGoroutine
	list := e.List()
	want := min(total, ringCap)
	if len(list) != want {
		t.Errorf("expected %d notifications, got %d", want, len(list))
	}
}

func TestDesktopNotificationFailureIgnored(t *testing.T) {
	e := New(nopEmit)
	e.desktopFn = func(_, _ string) error {
		return errors.New("osascript unavailable")
	}
	e.desktop = true

	// Should not panic; error is intentionally discarded.
	e.Send(LevelError, "fail", "desktop failed", "", "")

	if len(e.List()) != 1 {
		t.Error("notification should still be buffered despite desktop failure")
	}
}

func TestDesktopNotificationDisabled(t *testing.T) {
	called := false
	e := New(nopEmit)
	e.desktopFn = func(_, _ string) error {
		called = true
		return nil
	}

	e.SetDesktop(false)
	e.Send(LevelInfo, "t", "m", "", "")

	if called {
		t.Error("desktop notification should not be called when disabled")
	}
}

func TestDesktopNotificationEnabled(t *testing.T) {
	called := false
	e := New(nopEmit)
	e.desktopFn = func(_, _ string) error {
		called = true
		return nil
	}
	e.desktop = true

	e.Send(LevelInfo, "t", "m", "", "")

	if !called {
		t.Error("desktop notification should be called when enabled")
	}
}
