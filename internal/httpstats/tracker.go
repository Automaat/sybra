// Package httpstats tracks a coarse, in-memory sliding window of HTTP
// request outcomes so the monitor/health detectors can evaluate an
// HTTP-error-rate SLO without depending on the Prometheus scrape endpoint
// being enabled or scraped. It complements, not replaces, the Prometheus
// counters in internal/metrics: this package answers "what was the error
// rate over the last N minutes" in-process; Prometheus answers the same
// question for external alerting/dashboards.
package httpstats

import (
	"sync"
	"time"
)

// bucketWidth is the granularity of the sliding window ring. One-minute
// buckets keep iteration cheap on every monitor tick while still resolving a
// 15-minute SLO window precisely enough.
const bucketWidth = time.Minute

// maxBuckets bounds memory to a fixed ring regardless of request volume,
// covering up to a 1h lookback window.
const maxBuckets = 60

type bucket struct {
	start  time.Time
	total  int64
	errors int64
}

// Tracker accumulates HTTP request outcomes in fixed-width time buckets so a
// caller can compute "fraction of requests that errored in the last N
// minutes" without storing a growing, unbounded event log. Safe for
// concurrent use.
type Tracker struct {
	mu      sync.Mutex
	buckets []bucket // oldest first
}

// New returns an empty Tracker.
func New() *Tracker {
	return &Tracker{}
}

// Record adds one request outcome at now. isError should be true for
// responses that count against the error-rate SLO (conventionally 5xx).
func (t *Tracker) Record(now time.Time, isError bool) {
	now = now.Truncate(bucketWidth)
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.buckets)
	if n == 0 || t.buckets[n-1].start.Before(now) {
		t.buckets = append(t.buckets, bucket{start: now})
		n++
	}
	t.buckets[n-1].total++
	if isError {
		t.buckets[n-1].errors++
	}
	t.evictLocked(now)
}

// evictLocked drops buckets older than maxBuckets*bucketWidth relative to
// now. Must be called with mu held.
func (t *Tracker) evictLocked(now time.Time) {
	cutoff := now.Add(-maxBuckets * bucketWidth)
	i := 0
	for i < len(t.buckets) && t.buckets[i].start.Before(cutoff) {
		i++
	}
	if i > 0 {
		t.buckets = t.buckets[i:]
	}
}

// Snapshot is the aggregate request outcome over a window.
type Snapshot struct {
	Total     int
	Errors    int
	ErrorRate float64
}

// Snapshot sums bucket data over [now-window, now]. window is clamped to the
// tracker's retained history (maxBuckets*bucketWidth); a longer window simply
// returns everything currently retained. Buckets timestamped after now are
// excluded too — Record always uses the real wall clock in production, so
// this only matters for callers (e.g. tests) that query with a now older
// than some already-recorded data; without the upper bound such data would
// otherwise leak into every earlier query indefinitely.
func (t *Tracker) Snapshot(now time.Time, window time.Duration) Snapshot {
	cutoff := now.Add(-window)
	upper := now.Truncate(bucketWidth)
	t.mu.Lock()
	defer t.mu.Unlock()
	var total, errs int64
	for i := range t.buckets {
		if t.buckets[i].start.Before(cutoff) || t.buckets[i].start.After(upper) {
			continue
		}
		total += t.buckets[i].total
		errs += t.buckets[i].errors
	}
	snap := Snapshot{Total: int(total), Errors: int(errs)}
	if total > 0 {
		snap.ErrorRate = float64(errs) / float64(total)
	}
	return snap
}
