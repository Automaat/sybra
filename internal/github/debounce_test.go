package github

import (
	"testing"
	"time"
)

func TestIssueTracker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	tracker := NewIssueTracker(30 * time.Minute)
	tracker.now = func() time.Time { return now }

	t.Run("first occurrence is handleable", func(t *testing.T) {
		if !tracker.ShouldHandle("t1", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=true for new issue")
		}
	})

	t.Run("within cooldown is not handleable", func(t *testing.T) {
		tracker.MarkHandled("t1", PRIssueConflict)

		now = now.Add(10 * time.Minute)
		if tracker.ShouldHandle("t1", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=false within cooldown")
		}
	})

	t.Run("after cooldown is handleable again", func(t *testing.T) {
		now = now.Add(25 * time.Minute) // 35 min total since mark
		if !tracker.ShouldHandle("t1", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=true after cooldown")
		}
	})

	t.Run("different issue kinds are independent", func(t *testing.T) {
		now = time.Date(2026, 4, 3, 13, 0, 0, 0, time.UTC)
		tracker.MarkHandled("t2", PRIssueCIFailure)

		if !tracker.ShouldHandle("t2", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=true for different kind")
		}
		if tracker.ShouldHandle("t2", PRIssueCIFailure) {
			t.Fatal("expected ShouldHandle=false for same kind")
		}
	})

	t.Run("clear removes tracking and retries", func(t *testing.T) {
		tracker.Clear("t2", PRIssueCIFailure)
		if !tracker.ShouldHandle("t2", PRIssueCIFailure) {
			t.Fatal("expected ShouldHandle=true after Clear")
		}
		if tracker.Retries("t2", PRIssueCIFailure) != 0 {
			t.Fatal("expected retries=0 after Clear")
		}
	})

	t.Run("clear cooldown preserves retry count", func(t *testing.T) {
		tracker.MarkHandled("t4", PRIssueCIFailure)
		if tracker.Retries("t4", PRIssueCIFailure) != 1 {
			t.Fatalf("retries = %d, want 1", tracker.Retries("t4", PRIssueCIFailure))
		}

		tracker.ClearCooldown("t4", PRIssueCIFailure)
		if !tracker.ShouldHandle("t4", PRIssueCIFailure) {
			t.Fatal("expected ShouldHandle=true after ClearCooldown")
		}
		if tracker.Retries("t4", PRIssueCIFailure) != 1 {
			t.Fatal("expected retries preserved after ClearCooldown")
		}
	})

	t.Run("max retries blocks handling", func(t *testing.T) {
		now = time.Date(2026, 4, 3, 14, 0, 0, 0, time.UTC)
		for i := range maxRetries {
			tracker.MarkHandled("t5", PRIssueConflict)
			now = now.Add(31 * time.Minute)
			if i < maxRetries-1 {
				if !tracker.ShouldHandle("t5", PRIssueConflict) {
					t.Fatalf("expected ShouldHandle=true on retry %d", i+1)
				}
			}
		}
		// Even after cooldown, max retries should block.
		if tracker.ShouldHandle("t5", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=false after max retries")
		}
	})

	t.Run("clear resets retry cap", func(t *testing.T) {
		tracker.Clear("t5", PRIssueConflict)
		if !tracker.ShouldHandle("t5", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=true after Clear resets retries")
		}
	})

	t.Run("cleanup removes old entries", func(t *testing.T) {
		tracker.MarkHandled("t3", PRIssueConflict)
		now = now.Add(61 * time.Minute) // past 2x cooldown
		tracker.Cleanup()

		// t3 should be cleaned up
		if !tracker.ShouldHandle("t3", PRIssueConflict) {
			t.Fatal("expected ShouldHandle=true after cleanup")
		}
		if tracker.Retries("t3", PRIssueConflict) != 0 {
			t.Fatal("expected retries cleaned up")
		}
	})
}
