package logging

import (
	"log/slog"
	"sync"
)

// ErrorThrottle suppresses repeated identical error log entries that would
// otherwise dominate the log when a transient or unfixable failure recurs on
// every poller tick (e.g. todoist outage, restart-stale loop hitting an
// unrunnable task once a minute).
//
// On the first occurrence of a given (key, err) pair the throttle logs at
// ERROR. While the same error keeps recurring under that key the throttle
// downgrades subsequent entries to DEBUG. A different error message under the
// same key — or an explicit Clear after success — re-arms the ERROR log so
// state changes are never lost.
type ErrorThrottle struct {
	mu   sync.Mutex
	last map[string]string
}

// NewErrorThrottle returns an empty throttle ready for use.
func NewErrorThrottle() *ErrorThrottle {
	return &ErrorThrottle{last: make(map[string]string)}
}

// Log emits err under msg. The first occurrence (or any change in err.Error()
// for the given key) is logged at ERROR; identical repeats are logged at DEBUG.
// attrs are forwarded to slog as key/value pairs.
func (t *ErrorThrottle) Log(logger *slog.Logger, msg, key string, err error, attrs ...any) {
	if err == nil {
		t.Clear(key)
		return
	}
	cur := err.Error()
	t.mu.Lock()
	prev, seen := t.last[key]
	repeat := seen && prev == cur
	t.last[key] = cur
	t.mu.Unlock()

	out := make([]any, 0, len(attrs)+2)
	out = append(out, attrs...)
	out = append(out, "err", err)
	if repeat {
		logger.Debug(msg, out...)
		return
	}
	logger.Error(msg, out...)
}

// Clear forgets the last-error state for key. Call after a successful run so
// the next failure is logged at ERROR even if the message matches a stale
// suppressed value.
func (t *ErrorThrottle) Clear(key string) {
	t.mu.Lock()
	delete(t.last, key)
	t.mu.Unlock()
}

// InfoThrottle suppresses repeated identical informational log entries that
// would otherwise dominate the log when a benign, expected condition recurs
// on every poller tick (e.g. a workflow step skipped every ResumeStalled scan
// while it waits out a retry-after cooldown).
//
// On the first occurrence of a given (key, value) pair the throttle logs at
// INFO. While the same value keeps recurring under that key the throttle
// downgrades subsequent entries to DEBUG. A different value under the same
// key re-arms the INFO log so state changes are never lost.
type InfoThrottle struct {
	mu   sync.Mutex
	last map[string]string
}

// NewInfoThrottle returns an empty throttle ready for use.
func NewInfoThrottle() *InfoThrottle {
	return &InfoThrottle{last: make(map[string]string)}
}

// Log emits msg under key. The first occurrence (or any change in value for
// the given key) is logged at INFO; identical repeats are logged at DEBUG.
// attrs are forwarded to slog as key/value pairs.
func (t *InfoThrottle) Log(logger *slog.Logger, msg, key, value string, attrs ...any) {
	t.mu.Lock()
	prev, seen := t.last[key]
	repeat := seen && prev == value
	t.last[key] = value
	t.mu.Unlock()

	if repeat {
		logger.Debug(msg, attrs...)
		return
	}
	logger.Info(msg, attrs...)
}

// Clear forgets the last-value state for key so the next Log call re-arms
// the INFO log even if the value matches a stale suppressed entry.
func (t *InfoThrottle) Clear(key string) {
	t.mu.Lock()
	delete(t.last, key)
	t.mu.Unlock()
}
