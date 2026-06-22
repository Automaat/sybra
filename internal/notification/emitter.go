package notification

import (
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/google/uuid"
)

const ringCap = 100

// Emitter sends notifications via Wails events and optional desktop alerts.
type Emitter struct {
	emit      func(event string, data any)
	buffer    []Notification
	mu        sync.RWMutex
	desktop   bool
	desktopFn func(title, message string) error
}

// New creates an Emitter that broadcasts via the provided emit function.
func New(emit func(event string, data any)) *Emitter {
	return &Emitter{
		emit:      emit,
		buffer:    make([]Notification, 0, ringCap),
		desktop:   true,
		desktopFn: sendDesktopNotification,
	}
}

// Send creates and broadcasts a notification, honouring the desktop toggle.
func (e *Emitter) Send(level Level, title, message, taskID, agentID string) {
	e.mu.RLock()
	desktop := e.desktop
	e.mu.RUnlock()
	e.send(level, title, message, taskID, agentID, desktop)
}

// Alert sends an OS-native desktop notification regardless of the desktop
// toggle. For critical conditions (e.g. a stalled UI) that must reach the user
// even when the in-app event path is broken and the toggle is off.
func (e *Emitter) Alert(level Level, title, message string) {
	e.send(level, title, message, "", "", true)
}

func (e *Emitter) send(level Level, title, message, taskID, agentID string, desktop bool) {
	n := Notification{
		ID:        uuid.NewString(),
		Level:     level,
		Title:     title,
		Message:   message,
		TaskID:    taskID,
		AgentID:   agentID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e.mu.Lock()
	if len(e.buffer) >= ringCap {
		e.buffer = e.buffer[1:]
	}
	e.buffer = append(e.buffer, n)
	e.mu.Unlock()

	e.emit(events.Notification, n)

	if desktop {
		_ = e.desktopFn(title, message)
	}
}

// List returns all buffered notifications, newest first.
func (e *Emitter) List() []Notification {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]Notification, len(e.buffer))
	for i, n := range e.buffer {
		out[len(e.buffer)-1-i] = n
	}
	return out
}

// SetDesktop enables or disables macOS desktop notifications.
func (e *Emitter) SetDesktop(enabled bool) {
	e.mu.Lock()
	e.desktop = enabled
	e.mu.Unlock()
}
