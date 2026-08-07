package backoff

import (
	"testing"
	"time"
)

func TestForAttemptBoundsAndCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		attempt   int
		base      time.Duration
		ceiling   time.Duration
		wantMin   time.Duration
		wantMax   time.Duration
		atCeiling bool
	}{
		{name: "disabled attempt", attempt: 0, base: time.Second, ceiling: time.Minute},
		{name: "first", attempt: 1, base: time.Second, ceiling: time.Minute, wantMin: 500 * time.Millisecond, wantMax: time.Second},
		{name: "doubles", attempt: 3, base: time.Second, ceiling: time.Minute, wantMin: 2 * time.Second, wantMax: 4 * time.Second},
		{name: "caps", attempt: 20, base: time.Second, ceiling: time.Minute, wantMin: 30 * time.Second, wantMax: time.Minute, atCeiling: true},
		{name: "base above cap", attempt: 1, base: 2 * time.Minute, ceiling: time.Minute, wantMin: 30 * time.Second, wantMax: time.Minute, atCeiling: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ForAttempt(tt.attempt, tt.base, tt.ceiling)
			if got.Delay < tt.wantMin || got.Delay > tt.wantMax {
				t.Fatalf("ForAttempt delay = %v, want [%v, %v]", got.Delay, tt.wantMin, tt.wantMax)
			}
			if got.AtCeiling != tt.atCeiling {
				t.Fatalf("ForAttempt AtCeiling = %v, want %v", got.AtCeiling, tt.atCeiling)
			}
		})
	}
}

func TestForAttemptJitterEndpoints(t *testing.T) {
	t.Parallel()

	low := forAttempt(3, time.Second, time.Minute, func(int64) int64 { return 0 })
	high := forAttempt(3, time.Second, time.Minute, func(n int64) int64 { return n - 1 })
	if low.Delay != 2*time.Second || high.Delay != 4*time.Second {
		t.Fatalf("jitter endpoints = [%v, %v], want [2s, 4s]", low.Delay, high.Delay)
	}

	oddLow := forAttempt(1, 3*time.Nanosecond, time.Second, func(int64) int64 { return 0 })
	if oddLow.Delay != 2*time.Nanosecond {
		t.Fatalf("odd jitter minimum = %v, want ceiling-half 2ns", oddLow.Delay)
	}
}

func TestStepsForAttemptJittersInWholeSteps(t *testing.T) {
	t.Parallel()

	low := stepsForAttempt(2, 1, 8, func(int) int { return 0 })
	high := stepsForAttempt(2, 1, 8, func(n int) int { return n - 1 })
	if low.Steps != 1 || high.Steps != 2 {
		t.Fatalf("step jitter endpoints = [%d, %d], want [1, 2]", low.Steps, high.Steps)
	}
	oddLow := stepsForAttempt(1, 3, 10, func(int) int { return 0 })
	if oddLow.Steps != 2 {
		t.Fatalf("odd step jitter minimum = %d, want ceiling-half 2", oddLow.Steps)
	}

	maxInt := int(^uint(0) >> 1)
	capped := StepsForAttempt(100, 1, maxInt)
	if capped.Steps < maxInt/2 || capped.Steps > maxInt || !capped.AtCeiling {
		t.Fatalf("large capped step result = %+v, want [%d, %d] at ceiling", capped, maxInt/2, maxInt)
	}
}

func TestWithoutJitter(t *testing.T) {
	t.Parallel()

	got := WithoutJitter(4, time.Second, 5*time.Second, "deterministic protocol deadline")
	if got.Delay != 5*time.Second || !got.AtCeiling {
		t.Fatalf("WithoutJitter = %+v, want delay 5s at ceiling", got)
	}
}

func TestWithoutJitterRequiresReason(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("WithoutJitter did not panic for an empty reason")
			panic("unreachable")
		}
	}()
	WithoutJitter(1, time.Second, time.Minute, "  ")
}
