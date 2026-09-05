package bgop

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
)

// The completion TTL is five minutes. Before the clock seam this window could
// only be exercised by sleeping past it, so it was not exercised at all.
func TestListDropsCompletedOpsPastTheTTLWithoutSleeping(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	tracker := NewTracker(func(string, any) {}, NewFilePersistence(filepath.Join(t.TempDir(), "ops.json")), nil)
	tracker.SetClock(fake)

	done := tracker.Start(TypeClone, "finished", "owner/repo", "task-1")
	running := tracker.Start(TypeClone, "still going", "owner/repo", "task-2")
	tracker.Complete(done)

	if got := len(tracker.List()); got != 2 {
		t.Fatalf("List() = %d ops, want 2 while both are inside the TTL", got)
	}

	fake.Advance(completionTTL - time.Second)
	if got := len(tracker.List()); got != 2 {
		t.Fatalf("List() = %d ops one second before the TTL, want 2", got)
	}

	fake.Advance(2 * time.Second)
	got := tracker.List()
	if len(got) != 1 {
		t.Fatalf("List() = %d ops past the TTL, want 1", len(got))
	}
	if got[0].ID != running {
		t.Errorf("List() kept %q, want the still-running op %q", got[0].ID, running)
	}
}

// A failed op is subject to the same window as a completed one.
func TestListDropsFailedOpsPastTheTTL(t *testing.T) {
	fake := clock.NewFake(time.Time{})
	tracker := NewTracker(func(string, any) {}, NewFilePersistence(filepath.Join(t.TempDir(), "ops.json")), nil)
	tracker.SetClock(fake)

	id := tracker.Start(TypeClone, "doomed", "owner/repo", "task-1")
	tracker.Fail(id, errors.New("boom"))

	if got := len(tracker.List()); got != 1 {
		t.Fatalf("List() = %d ops, want the failed op still inside the TTL", got)
	}
	fake.Advance(completionTTL + time.Second)
	if got := len(tracker.List()); got != 0 {
		t.Errorf("List() = %d ops past the TTL, want 0", got)
	}
}

func TestTrackerDefaultsToTheSystemClock(t *testing.T) {
	tracker := NewTracker(func(string, any) {}, NewFilePersistence(filepath.Join(t.TempDir(), "ops.json")), nil)
	before := time.Now().UTC()
	id := tracker.Start(TypeClone, "now", "owner/repo", "task-1")
	after := time.Now().UTC()

	ops := tracker.List()
	if len(ops) != 1 || ops[0].ID != id {
		t.Fatalf("List() = %+v, want the started op", ops)
	}
	if ops[0].StartedAt.Before(before) || ops[0].StartedAt.After(after) {
		t.Errorf("StartedAt = %v, want between %v and %v", ops[0].StartedAt, before, after)
	}
}
