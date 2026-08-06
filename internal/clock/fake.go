package clock

import (
	"sync"
	"time"
)

// Fake is a hand-driven clock for tests. It is safe for concurrent use: the
// code under test often reads the time from a background goroutine while the
// test advances it, and a data race there would be the test's own bug rather
// than the subject's.
//
// It lives beside the production code rather than in a _test.go file because
// several packages need it, and Go cannot import another package's tests.
type Fake struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFake returns a Fake started at t. A zero t starts at a fixed, arbitrary
// instant rather than the zero time, so a test that formats or subtracts gets
// a plausible date instead of year 1.
func NewFake(t time.Time) *Fake {
	if t.IsZero() {
		t = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	}
	return &Fake{now: t}
}

// Now returns the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Advance moves the clock forward by d and returns the new time. A negative d
// moves it back, which is how to exercise a peer or filesystem timestamp from
// the future.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

// Set moves the clock to t and returns it.
func (f *Fake) Set(t time.Time) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
	return f.now
}
