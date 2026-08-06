package clock

import (
	"sync"
	"testing"
	"time"
)

func TestSystemAdvancesWithTheWallClock(t *testing.T) {
	t.Parallel()
	before := time.Now()
	got := System{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("System.Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestOrDefaultsToSystem(t *testing.T) {
	t.Parallel()
	if _, ok := Or(nil).(System); !ok {
		t.Errorf("Or(nil) = %T, want System", Or(nil))
	}
	f := NewFake(time.Time{})
	if got := Or(f); got != Clock(f) {
		t.Errorf("Or(fake) = %v, want the fake back", got)
	}
}

func TestFakeOnlyMovesWhenDriven(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	f := NewFake(start)

	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	if got := f.Now(); !got.Equal(start) {
		t.Errorf("a second read moved the clock: %v", got)
	}
	if got := f.Advance(90 * time.Minute); !got.Equal(start.Add(90 * time.Minute)) {
		t.Errorf("Advance returned %v, want %v", got, start.Add(90*time.Minute))
	}
	if got := f.Advance(-30 * time.Minute); !got.Equal(start.Add(time.Hour)) {
		t.Errorf("negative Advance returned %v, want %v", got, start.Add(time.Hour))
	}
	if got := f.Set(start); !got.Equal(start) {
		t.Errorf("Set returned %v, want %v", got, start)
	}
}

// A zero start would make a test that subtracts or formats produce year 1,
// which reads as a bug in the subject rather than an unset fixture.
func TestNewFakeZeroTimeStartsAtAPlausibleInstant(t *testing.T) {
	t.Parallel()
	got := NewFake(time.Time{}).Now()
	if got.IsZero() {
		t.Fatal("NewFake(zero) left the clock at the zero time")
	}
	if got.Year() < 2000 {
		t.Errorf("NewFake(zero) = %v, want a plausible modern instant", got)
	}
}

// The subject often reads the time from a background goroutine while the test
// advances it, so a race here would be the harness's fault, not the subject's.
func TestFakeIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Time{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); f.Advance(time.Second) }()
		go func() { defer wg.Done(); _ = f.Now() }()
	}
	wg.Wait()
	if got, want := f.Now(), NewFake(time.Time{}).Now().Add(8*time.Second); !got.Equal(want) {
		t.Errorf("after 8 concurrent Advances Now() = %v, want %v", got, want)
	}
}
