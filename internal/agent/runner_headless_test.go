package agent

import (
	"context"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/tmux"
)

func testManagerWithEmit(t *testing.T, emit EmitFunc) *Manager {
	t.Helper()
	return NewManager(t.Context(), tmux.NewManager(), emit, discardLogger(), t.TempDir())
}

func TestHandleError(t *testing.T) {
	var emittedEvent string
	var emittedData any
	emit := func(event string, data any) {
		emittedEvent = event
		emittedData = data
	}

	m := testManagerWithEmit(t, emit)

	a := &Agent{
		ID:    "test-123",
		State: StateRunning,
	}

	m.handleError(a, errTestSentinel)

	if a.State != StateStopped {
		t.Errorf("State = %q, want %q", a.State, StateStopped)
	}

	wantEvent := events.AgentError("test-123")
	if emittedEvent != wantEvent {
		t.Errorf("event = %q, want %q", emittedEvent, wantEvent)
	}

	msg, ok := emittedData.(string)
	if !ok {
		t.Fatalf("emitted data type = %T, want string", emittedData)
	}
	if msg != "test error" {
		t.Errorf("error message = %q, want %q", msg, "test error")
	}
}

var errTestSentinel = sentinelError("test error")

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

func TestRunHeadlessFailsToStart(t *testing.T) {
	var lastEvent string
	emit := func(event string, _ any) {
		lastEvent = event
	}

	m := testManagerWithEmit(t, emit)

	a := &Agent{
		ID:    "test-headless",
		State: StateRunning,
	}

	m.runHeadless(t.Context(), a, "test prompt", nil)

	if a.State != StateStopped {
		t.Errorf("State = %q, want %q", a.State, StateStopped)
	}

	if lastEvent != events.AgentError("test-headless") && lastEvent != events.AgentState("test-headless") {
		t.Errorf("last event = %q, want error or state event", lastEvent)
	}
}

func TestRunHeadlessWithAllowedTools(t *testing.T) {
	m := testManagerWithEmit(t, func(string, any) {})

	a := &Agent{
		ID:    "test-tools",
		State: StateRunning,
	}

	m.runHeadless(t.Context(), a, "test prompt", []string{"Read", "Write"})

	if a.State != StateStopped {
		t.Errorf("State = %q, want %q", a.State, StateStopped)
	}
}

func TestRunHeadlessWithResume(t *testing.T) {
	m := testManagerWithEmit(t, func(string, any) {})

	a := &Agent{
		ID:        "test-resume",
		State:     StateRunning,
		SessionID: "prev-session-id",
	}

	m.runHeadless(t.Context(), a, "test prompt", nil)

	if a.State != StateStopped {
		t.Errorf("State = %q, want %q", a.State, StateStopped)
	}
}

func TestRunHeadlessCancelledContext(t *testing.T) {
	// Use an already-expired deadline to simulate a cancelled context
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	m := NewManager(ctx, tmux.NewManager(), func(string, any) {}, discardLogger(), t.TempDir())

	a := &Agent{
		ID:    "test-cancelled",
		State: StateRunning,
	}

	m.runHeadless(ctx, a, "test prompt", nil)

	if a.State != StateStopped {
		t.Errorf("State = %q, want %q", a.State, StateStopped)
	}
}
