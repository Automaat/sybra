package logging

import (
	"log/slog"
	"sync"
	"time"
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

// IntervalThrottle promotes a condition that would otherwise log at DEBUG on
// every poll tick (a dropped completion, a resume-loop skip reason) to INFO
// at most once per key per interval, so a live incident leaves a visible
// trail without one INFO line per tick for its whole duration.
type IntervalThrottle struct {
	mu       sync.Mutex
	last     map[string]time.Time
	interval time.Duration
}

// NewIntervalThrottle returns a throttle that logs at most once per key per
// interval at INFO, downgrading more frequent repeats to DEBUG.
func NewIntervalThrottle(interval time.Duration) *IntervalThrottle {
	return &IntervalThrottle{last: make(map[string]time.Time), interval: interval}
}

// Log emits msg under key: INFO the first time (or once interval has
// elapsed since the last INFO for this key), DEBUG otherwise.
func (t *IntervalThrottle) Log(logger *slog.Logger, msg, key string, attrs ...any) {
	now := time.Now()
	t.mu.Lock()
	last, seen := t.last[key]
	due := !seen || now.Sub(last) >= t.interval
	if due {
		t.last[key] = now
	}
	t.mu.Unlock()

	if due {
		logger.Info(msg, attrs...)
		return
	}
	logger.Debug(msg, attrs...)
}
