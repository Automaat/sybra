package github

import (
	"errors"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
)

// The circuit breaker's base backoff is 30 seconds and it doubles to five
// minutes. Before the clock seam, reaching the far end of that schedule in a
// test meant sleeping for minutes, so the schedule went untested.
func TestAuthCircuitBackoffScheduleWithoutSleeping(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	setAuthHealthClockForTest(fake)
	resetAuthHealthForTest()
	t.Cleanup(func() {
		setAuthHealthClockForTest(nil)
		resetAuthHealthForTest()
	})

	authFail := errors.New("gh: Bad credentials")

	tests := []struct {
		name        string
		wantBackoff time.Duration
	}{
		{name: "first failure", wantBackoff: authCircuitBaseBackoff},
		{name: "second doubles", wantBackoff: 2 * authCircuitBaseBackoff},
		{name: "third doubles again", wantBackoff: 4 * authCircuitBaseBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ObserveCallResult([]byte("Bad credentials"), authFail)

			open, retryAfter := AuthCircuitOpen()
			if !open {
				t.Fatal("circuit should be open right after a failure")
			}
			if got := retryAfter.Sub(fake.Now()); got != tt.wantBackoff {
				t.Fatalf("retryAfter is %v out, want %v", got, tt.wantBackoff)
			}

			// One tick short of the window the circuit is still open; one tick
			// past it, closed — the boundary a sleep-based test cannot pin.
			fake.Advance(tt.wantBackoff - time.Nanosecond)
			if open, _ := AuthCircuitOpen(); !open {
				t.Error("circuit closed one nanosecond early")
			}
			fake.Advance(time.Nanosecond)
			if open, _ := AuthCircuitOpen(); open {
				t.Error("circuit still open at the retry deadline")
			}
		})
	}
}

// The schedule is bounded: doubling must stop at the five-minute ceiling
// rather than growing without limit. Each failure only counts once the
// previous window has elapsed — while the circuit is open, a duplicate
// observation deliberately leaves the step alone (see observeFailure), so a
// concurrent burst cannot jump straight to the cap.
func TestAuthCircuitBackoffStopsAtTheCeiling(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	setAuthHealthClockForTest(fake)
	resetAuthHealthForTest()
	t.Cleanup(func() {
		setAuthHealthClockForTest(nil)
		resetAuthHealthForTest()
	})

	authFail := errors.New("gh: Bad credentials")
	for range 20 {
		ObserveCallResult([]byte("Bad credentials"), authFail)
		_, retryAfter := AuthCircuitOpen()
		fake.Set(retryAfter)
	}
	ObserveCallResult([]byte("Bad credentials"), authFail)

	_, retryAfter := AuthCircuitOpen()
	if got := retryAfter.Sub(fake.Now()); got != authCircuitMaxBackoff {
		t.Errorf("backoff after 20 failures = %v, want the %v ceiling", got, authCircuitMaxBackoff)
	}
}
