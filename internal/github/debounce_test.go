package github

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// TestIssueTracker_DecideSignature locks the loop fix: a fix agent that keeps
// pushing new commits against the SAME unchanged feedback signature exhausts the
// budget (so the monitor escalates instead of looping), while genuinely new
// reviewer feedback resets it.
func TestIssueTracker_DecideSignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	tr := NewIssueTracker(30 * time.Minute)
	tr.now = func() time.Time { return now }

	if got := tr.Decide("t1", PRIssueComments, "sha1", "sigA"); got != DispatchHandle {
		t.Fatalf("first dispatch = %v, want Handle", got)
	}
	tr.MarkHandled("t1", PRIssueComments, "sha1")

	if got := tr.Decide("t1", PRIssueComments, "sha1", "sigA"); got != DispatchSkip {
		t.Fatalf("same sha/sig within cooldown = %v, want Skip", got)
	}

	// A new commit each round (defeating the old SHA-only cap) must NOT keep the
	// loop alive: the unchanged sig caps the budget.
	for i := 1; i < MaxRetries; i++ {
		now = now.Add(31 * time.Minute)
		sha := "sha" + strconv.Itoa(i+1)
		if got := tr.Decide("t1", PRIssueComments, sha, "sigA"); got != DispatchHandle {
			t.Fatalf("round %d = %v, want Handle", i, got)
		}
		tr.MarkHandled("t1", PRIssueComments, sha)
	}

	now = now.Add(31 * time.Minute)
	if got := tr.Decide("t1", PRIssueComments, "sha-new", "sigA"); got != DispatchExhausted {
		t.Fatalf("budget spent on unchanged sig = %v, want Exhausted", got)
	}

	// Genuinely new feedback (different sig) resets the budget.
	if got := tr.Decide("t1", PRIssueComments, "sha-new", "sigB"); got != DispatchHandle {
		t.Fatalf("new feedback = %v, want Handle", got)
	}
}

func TestIssueTracker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	tracker := NewIssueTracker(30 * time.Minute)
	tracker.now = func() time.Time { return now }

	t.Run("first occurrence is handleable", func(t *testing.T) {
		if !tracker.ShouldHandle("t1", PRIssueConflict, "") {
			t.Fatal("expected ShouldHandle=true for new issue")
		}
	})

	t.Run("within cooldown is not handleable", func(t *testing.T) {
		tracker.MarkHandled("t1", PRIssueConflict, "sha1")

		now = now.Add(10 * time.Minute)
		if tracker.ShouldHandle("t1", PRIssueConflict, "sha1") {
			t.Fatal("expected ShouldHandle=false within cooldown")
		}
	})

	t.Run("same sha blocks regardless of cooldown", func(t *testing.T) {
		now = now.Add(25 * time.Minute) // 35 min total since mark — past cooldown
		if tracker.ShouldHandle("t1", PRIssueConflict, "sha1") {
			t.Fatal("expected ShouldHandle=false: same SHA as last attempt")
		}
	})

	t.Run("new sha allows retry after cooldown", func(t *testing.T) {
		if !tracker.ShouldHandle("t1", PRIssueConflict, "sha2") {
			t.Fatal("expected ShouldHandle=true: new SHA after cooldown")
		}
	})

	t.Run("new sha within cooldown is still blocked", func(t *testing.T) {
		tracker.MarkHandled("t6", PRIssueCIFailure, "sha-a")
		now = now.Add(5 * time.Minute) // within cooldown
		if tracker.ShouldHandle("t6", PRIssueCIFailure, "sha-b") {
			t.Fatal("expected ShouldHandle=false: new SHA but within cooldown")
		}
	})

	t.Run("empty sha skips sha gate", func(t *testing.T) {
		now = now.Add(31 * time.Minute) // past cooldown for t6
		if !tracker.ShouldHandle("t6", PRIssueCIFailure, "") {
			t.Fatal("expected ShouldHandle=true: empty SHA skips SHA gate")
		}
	})

	t.Run("different issue kinds are independent", func(t *testing.T) {
		now = time.Date(2026, 4, 3, 13, 0, 0, 0, time.UTC)
		tracker.MarkHandled("t2", PRIssueCIFailure, "sha-x")

		if !tracker.ShouldHandle("t2", PRIssueConflict, "sha-x") {
			t.Fatal("expected ShouldHandle=true for different kind")
		}
		if tracker.ShouldHandle("t2", PRIssueCIFailure, "sha-x") {
			t.Fatal("expected ShouldHandle=false for same kind + same SHA")
		}
	})

	t.Run("clear removes tracking and retries", func(t *testing.T) {
		tracker.Clear("t2", PRIssueCIFailure)
		if !tracker.ShouldHandle("t2", PRIssueCIFailure, "sha-x") {
			t.Fatal("expected ShouldHandle=true after Clear")
		}
		if tracker.Retries("t2", PRIssueCIFailure) != 0 {
			t.Fatal("expected retries=0 after Clear")
		}
	})

	t.Run("clear cooldown preserves retry count and sha", func(t *testing.T) {
		tracker.MarkHandled("t4", PRIssueCIFailure, "sha-old")
		if tracker.Retries("t4", PRIssueCIFailure) != 1 {
			t.Fatalf("retries = %d, want 1", tracker.Retries("t4", PRIssueCIFailure))
		}

		tracker.ClearCooldown("t4", PRIssueCIFailure)
		// same SHA still blocked even after ClearCooldown
		if tracker.ShouldHandle("t4", PRIssueCIFailure, "sha-old") {
			t.Fatal("expected ShouldHandle=false: same SHA blocks even after ClearCooldown")
		}
		// new SHA allowed since time gate is cleared
		if !tracker.ShouldHandle("t4", PRIssueCIFailure, "sha-new") {
			t.Fatal("expected ShouldHandle=true: new SHA after ClearCooldown")
		}
		if tracker.Retries("t4", PRIssueCIFailure) != 1 {
			t.Fatal("expected retries preserved after ClearCooldown")
		}
	})

	t.Run("max retries blocks handling", func(t *testing.T) {
		now = time.Date(2026, 4, 3, 14, 0, 0, 0, time.UTC)
		sha := "sha-0"
		for i := range maxRetries {
			tracker.MarkHandled("t5", PRIssueConflict, sha)
			sha = "sha-" + string(rune('1'+i)) // new SHA each iteration
			now = now.Add(31 * time.Minute)
			if i < maxRetries-1 {
				if !tracker.ShouldHandle("t5", PRIssueConflict, sha) {
					t.Fatalf("expected ShouldHandle=true on retry %d", i+1)
				}
			}
		}
		// Even after cooldown with new SHA, max retries should block.
		if tracker.ShouldHandle("t5", PRIssueConflict, sha) {
			t.Fatal("expected ShouldHandle=false after max retries")
		}
	})

	t.Run("clear resets retry cap", func(t *testing.T) {
		tracker.Clear("t5", PRIssueConflict)
		if !tracker.ShouldHandle("t5", PRIssueConflict, "") {
			t.Fatal("expected ShouldHandle=true after Clear resets retries")
		}
	})

	t.Run("cleanup drops cooldown but never forgets the last SHA", func(t *testing.T) {
		// Regression for #1528: a long-lived PR whose head never changes must
		// keep skipping dispatch even once the time-based cooldown record ages
		// out of Cleanup — otherwise the tracker "forgets" it already handled
		// this exact commit and re-dispatches a fresh pr-fix agent forever.
		tracker.MarkHandled("t3", PRIssueConflict, "sha-old")
		now = now.Add(61 * time.Minute) // past 2x cooldown
		tracker.Cleanup()

		// Same SHA still blocks — Cleanup must not erase the SHA memory.
		if tracker.ShouldHandle("t3", PRIssueConflict, "sha-old") {
			t.Fatal("expected ShouldHandle=false: same SHA still blocks after cleanup")
		}
		if tracker.Retries("t3", PRIssueConflict) != 1 {
			t.Fatalf("expected retries preserved at 1 after cleanup, got %d", tracker.Retries("t3", PRIssueConflict))
		}
		// A genuinely new commit is still handled promptly.
		if !tracker.ShouldHandle("t3", PRIssueConflict, "sha-new") {
			t.Fatal("expected ShouldHandle=true: new SHA allowed after cleanup")
		}
	})

	t.Run("cleanup preserves retries at cap across long gaps", func(t *testing.T) {
		// Simulate maxRetries attempts, each on a new SHA and after cooldown.
		now = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		sha := "cap-sha-0"
		for i := range maxRetries {
			tracker.MarkHandled("t8", PRIssueCIFailure, sha)
			sha = fmt.Sprintf("cap-sha-%d", i+1)
			now = now.Add(31 * time.Minute)
		}
		// Cleanup runs after 2x cooldown — but retry counter must survive.
		now = now.Add(61 * time.Minute)
		tracker.Cleanup()

		// AtCap must still be true so the caller can escalate.
		if !tracker.AtCap("t8", PRIssueCIFailure) {
			t.Fatal("expected AtCap=true after cleanup with capped entry")
		}
		if tracker.Retries("t8", PRIssueCIFailure) != maxRetries {
			t.Fatalf("expected retries=%d after cleanup, got %d", maxRetries, tracker.Retries("t8", PRIssueCIFailure))
		}
		// ShouldHandle must still be false.
		if tracker.ShouldHandle("t8", PRIssueCIFailure, sha) {
			t.Fatal("expected ShouldHandle=false: still at cap after cleanup")
		}
	})
}
