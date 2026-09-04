// Package clock is the seam that lets time-dependent logic be tested without
// sleeping.
//
// Every deadline, backoff window, staleness check and rate-limit cycle in this
// tree read the wall clock directly, so a test could only exercise them by
// sleeping or by racing. That is a standing structural cause of the flakiness
// this repo keeps re-diagnosing one test at a time, and why the e2e suite
// needs timeout scaling to stay green under load.
//
// The interface is deliberately narrow: a package that only stamps CreatedAt
// or UpdatedAt does not need it and should keep calling time.Now(). Reach for
// a clock when time drives control flow.
package clock

import "time"

// Clock reports the current time. Production code takes System; tests take a
// *Fake and step it.
type Clock interface {
	Now() time.Time
}

// System reads the wall clock.
type System struct{}

// Now returns the current wall-clock time.
func (System) Now() time.Time { return time.Now() }

// Or returns c, or System when c is nil. Constructors call this so a caller
// that does not care about time can pass nil and still get a working value —
// keeping the seam from leaking into every call site.
func Or(c Clock) Clock {
	if c == nil {
		return System{}
	}
	return c
}
