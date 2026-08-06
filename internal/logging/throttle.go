package logging

import (
	"log/slog"
	"sync"
	"time"
)

// ErrorThrottle suppresses repeated identical error log entries that would
// otherwise dominate the log when a transient or unfixable failure recurs on
// every poller tick (e.g. a GitHub outage, restart-stale loop hitting an
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

// InfoRepeatInterval bounds how often an unchanged repeat is re-emitted at
// DEBUG. Downgrading a repeat is not enough on its own: a condition that holds
// for days still writes one line per poller tick at debug level — a provider
// parked for 60 hours produces roughly 3,600 identical lines per task at the
// default 60s maintenance interval. Re-emitting on an interval keeps the
// "still true" signal without the volume.
const InfoRepeatInterval = 30 * time.Minute

// InfoThrottle suppresses repeated identical informational log entries that
// would otherwise dominate the log when a benign, expected condition recurs
// on every poller tick (e.g. a workflow step skipped every ResumeStalled scan
// while it waits out a retry-after cooldown, or a provider parked for days
// behind a provider-stated reset instant).
//
// On the first occurrence of a given (key, value) pair the throttle logs at
// INFO. While the same value keeps recurring under that key the throttle
// drops subsequent entries, re-emitting at DEBUG at most once per
// InfoRepeatInterval. A different value under the same key re-arms the INFO
// log immediately, so state changes are never lost or delayed.
type InfoThrottle struct {
	mu   sync.Mutex
	last map[string]infoEntry
	now  func() time.Time
}

type infoEntry struct {
	value    string
	loggedAt time.Time
}

// NewInfoThrottle returns an empty throttle ready for use.
func NewInfoThrottle() *InfoThrottle {
	return &InfoThrottle{last: make(map[string]infoEntry), now: time.Now}
}

// Log emits msg under key. The first occurrence (or any change in value for
// the given key) is logged at INFO. An unchanged repeat is emitted at DEBUG
// only if InfoRepeatInterval has elapsed since the last emission, and is
// otherwise dropped. attrs are forwarded to slog as key/value pairs.
func (t *InfoThrottle) Log(logger *slog.Logger, msg, key, value string, attrs ...any) {
	now := t.now()
	t.mu.Lock()
	prev, seen := t.last[key]
	repeat := seen && prev.value == value
	quiet := repeat && now.Sub(prev.loggedAt) < InfoRepeatInterval
	if !quiet {
		t.last[key] = infoEntry{value: value, loggedAt: now}
	}
	t.mu.Unlock()

	switch {
	case quiet:
		return
	case repeat:
		logger.Debug(msg, attrs...)
	default:
		logger.Info(msg, attrs...)
	}
}

// Clear forgets the last-value state for key so the next Log call re-arms
// the INFO log even if the value matches a stale suppressed entry.
func (t *InfoThrottle) Clear(key string) {
	t.mu.Lock()
	delete(t.last, key)
	t.mu.Unlock()
}
