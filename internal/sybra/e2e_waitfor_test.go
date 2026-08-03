//go:build e2e

package sybra

import (
	"testing"
	"time"
)

// TestAwaitConditionCountsIntendedTimeNotWallClock is #2811's core property.
// The host is reported idle (scale 1) while every poll costs far more
// wall-clock than the interval asked for — what a process-spawn/IO-bound suite
// looks like on darwin, where load-per-CPU stays under 1 and the load-average
// scaler grants no headroom at all.
//
// The wait must still give the condition its full number of chances, because
// the budget is spent in intended time rather than wall-clock.
func TestAwaitConditionCountsIntendedTimeNotWallClock(t *testing.T) {
	const succeedOnCall = 5
	calls := 0
	budget, _, ok := awaitCondition(300*time.Millisecond, func() bool {
		calls++
		time.Sleep(120 * time.Millisecond) // each poll far outruns the 50ms interval
		return calls >= succeedOnCall
	}, func() int64 { return 1 })

	if !ok {
		t.Fatalf("gave up after %s having polled %d times; a slow host must not cost the condition its chances", budget, calls)
	}
	if calls != succeedOnCall {
		t.Fatalf("condition ran %d times, want %d", calls, succeedOnCall)
	}
}

// TestAwaitConditionGivesUpWhenConditionNeverHolds is the counterweight: a
// condition that never becomes true must still fail, and in bounded time, or
// counting intended time would turn a genuine hang into an unbounded wait.
func TestAwaitConditionGivesUpWhenConditionNeverHolds(t *testing.T) {
	start := time.Now()
	_, _, ok := awaitCondition(200*time.Millisecond,
		func() bool { return false },
		func() int64 { return 1 })
	if ok {
		t.Fatal("a never-satisfied condition must not report success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("gave up only after %s; the budget is not bounded", elapsed)
	}
}

// TestAwaitConditionHonoursTheScale pins that the injected scaler still sizes
// the budget: a larger scale must buy proportionally more polls.
func TestAwaitConditionHonoursTheScale(t *testing.T) {
	count := func(scale int64) int {
		calls := 0
		awaitCondition(100*time.Millisecond, func() bool {
			calls++
			return false
		}, func() int64 { return scale })
		return calls
	}
	few, many := count(1), count(4)
	if many <= few {
		t.Fatalf("scale 4 polled %d times, scale 1 polled %d; a larger scale must buy more chances", many, few)
	}
}
