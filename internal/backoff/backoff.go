// Package backoff provides the shared bounded exponential retry policy.
package backoff

import (
	"math/rand/v2"
	"strings"
	"time"
)

// Result is the delay selected for an attempt and whether the unjittered
// exponential schedule has reached its ceiling.
type Result struct {
	Delay     time.Duration
	AtCeiling bool
}

// StepsResult is the discrete counterpart of Result for schedulers whose wait
// is measured in whole ticks rather than wall-clock time.
type StepsResult struct {
	Steps     int
	AtCeiling bool
}

// ForAttempt returns a bounded exponential delay with equal jitter. Attempts
// are one-based: attempt 1 uses base, attempt 2 uses 2*base, and so on. Equal
// jitter selects uniformly from the upper half of the computed window, which
// keeps retries spread out even after the exponential schedule reaches its
// ceiling.
func ForAttempt(attempt int, base, ceiling time.Duration) Result {
	return forAttempt(attempt, base, ceiling, rand.Int64N)
}

// StepsForAttempt returns a bounded exponential whole-step wait with equal
// jitter. Sampling in the discrete domain keeps short schedules spread out;
// converting a continuous duration to ticks would collapse most values onto
// the same rounded tick.
func StepsForAttempt(attempt, base, ceiling int) StepsResult {
	return stepsForAttempt(attempt, base, ceiling, rand.IntN)
}

// WithoutJitter returns the exact bounded exponential delay. Callers must
// state why synchronized retries are safe; this makes every opt-out visible at
// the call site during review.
func WithoutJitter(attempt int, base, ceiling time.Duration, reason string) Result {
	if strings.TrimSpace(reason) == "" {
		panic("backoff: no-jitter reason is required")
	}
	nominal, atCeiling := exponential(attempt, base, ceiling)
	return Result{Delay: nominal, AtCeiling: atCeiling}
}

func forAttempt(attempt int, base, ceiling time.Duration, int64n func(int64) int64) Result {
	nominal, atCeiling := exponential(attempt, base, ceiling)
	if nominal <= 1 {
		return Result{Delay: nominal, AtCeiling: atCeiling}
	}
	minimum := nominal/2 + nominal%2
	span := int64(nominal-minimum) + 1
	return Result{
		Delay:     minimum + time.Duration(int64n(span)),
		AtCeiling: atCeiling,
	}
}

func stepsForAttempt(attempt, base, ceiling int, intn func(int) int) StepsResult {
	nominal, atCeiling := exponentialSteps(attempt, base, ceiling)
	if nominal <= 1 {
		return StepsResult{Steps: nominal, AtCeiling: atCeiling}
	}
	minimum := nominal/2 + nominal%2
	return StepsResult{
		Steps:     minimum + intn(nominal-minimum+1),
		AtCeiling: atCeiling,
	}
}

func exponential(attempt int, base, ceiling time.Duration) (time.Duration, bool) {
	if attempt <= 0 || base <= 0 || ceiling <= 0 {
		return 0, false
	}
	if base >= ceiling {
		return ceiling, true
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay > ceiling/2 {
			return ceiling, true
		}
		delay *= 2
		if delay >= ceiling {
			return ceiling, true
		}
	}
	return delay, false
}

func exponentialSteps(attempt, base, ceiling int) (int, bool) {
	if attempt <= 0 || base <= 0 || ceiling <= 0 {
		return 0, false
	}
	if base >= ceiling {
		return ceiling, true
	}
	steps := base
	for i := 1; i < attempt; i++ {
		if steps > ceiling/2 {
			return ceiling, true
		}
		steps *= 2
		if steps >= ceiling {
			return ceiling, true
		}
	}
	return steps, false
}
