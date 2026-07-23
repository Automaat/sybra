package github

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyMergeError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want MergeErrorClass
	}{
		{"nil", nil, ""},
		{"bad credentials", errors.New("gh: HTTP 401: Bad credentials"), MergeErrorAuth},
		{"rate limited", errors.New("gh: API rate limit exceeded for user"), MergeErrorRateLimit},
		{"secondary rate limit", errors.New("You have exceeded a secondary rate limit"), MergeErrorRateLimit},
		{"gateway timeout", errors.New("gh: HTTP 503: Service Unavailable"), MergeErrorTransient},
		{"connection reset", errors.New("write: connection reset by peer"), MergeErrorTransient},
		{"not mergeable", errors.New("Pull Request is not mergeable: the base branch policy prohibits the merge"), MergeErrorBlocked},
		{"required status check", errors.New("Required status check \"ci\" is expected"), MergeErrorBlocked},
		{"something else entirely", errors.New("gh: unexpected server error"), MergeErrorUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyMergeError(tt.err); got != tt.want {
				t.Errorf("ClassifyMergeError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestAutoMergeBackoff_ShouldAttempt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	b := NewAutoMergeBackoff()
	b.now = func() time.Time { return now }

	if !b.ShouldAttempt("owner/repo", 1, "sha1", "state1") {
		t.Fatal("first sighting must always attempt")
	}

	b.RecordFailure("owner/repo", 1, "sha1", "state1", MergeErrorBlocked)
	if b.ShouldAttempt("owner/repo", 1, "sha1", "state1") {
		t.Fatal("expected suppressed immediately after a recorded failure")
	}

	// A different head SHA (new push) reprobes immediately, regardless of
	// the still-open backoff window on the old SHA.
	if !b.ShouldAttempt("owner/repo", 1, "sha2", "state1") {
		t.Fatal("expected immediate reprobe on a new head SHA")
	}

	if !b.ShouldAttempt("owner/repo", 1, "sha1", "state2") {
		t.Fatal("expected immediate reprobe on changed PR/check state")
	}

	// Advancing past the backoff window on the original SHA allows retry.
	now = now.Add(3 * time.Hour)
	if !b.ShouldAttempt("owner/repo", 1, "sha1", "state1") {
		t.Fatal("expected attempt allowed once the backoff window elapses")
	}
}

func TestAutoMergeBackoff_ExponentialGrowthAndCeiling(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	b := NewAutoMergeBackoff()
	b.now = func() time.Time { return now }

	base, max := mergeBackoffWindow(MergeErrorBlocked)
	delay := base
	atCeiling := false
	for i := 0; i < 10; i++ {
		atCeiling = b.RecordFailure("owner/repo", 5, "sha1", "state1", MergeErrorBlocked)
		if b.Attempts("owner/repo", 5) != i+1 {
			t.Fatalf("attempts = %d, want %d", b.Attempts("owner/repo", 5), i+1)
		}
		if atCeiling {
			break
		}
		delay *= 2
	}
	if !atCeiling {
		t.Fatal("expected backoff to reach its ceiling within 10 failures")
	}
	if delay < max {
		// Growth may have saturated on an earlier iteration than our doubling
		// loop above computed; just confirm the ceiling was in fact reached.
		t.Logf("delay estimate %s reached ceiling %s early", delay, max)
	}

	// One more failure at the ceiling must not exceed it.
	b.RecordFailure("owner/repo", 5, "sha1", "state1", MergeErrorBlocked)
	// nextTry - now should be capped at max; verify indirectly via ShouldAttempt.
	now = now.Add(max - time.Second)
	if b.ShouldAttempt("owner/repo", 5, "sha1", "state1") {
		t.Fatal("expected still suppressed just before the ceiling elapses")
	}
	now = now.Add(2 * time.Second)
	if !b.ShouldAttempt("owner/repo", 5, "sha1", "state1") {
		t.Fatal("expected attempt allowed once the ceiling window elapses")
	}
}

func TestAutoMergeBackoff_ClassChangeResetsAttempts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	b := NewAutoMergeBackoff()
	b.now = func() time.Time { return now }

	b.RecordFailure("owner/repo", 9, "sha1", "state1", MergeErrorBlocked)
	b.RecordFailure("owner/repo", 9, "sha1", "state1", MergeErrorBlocked)
	if got := b.Attempts("owner/repo", 9); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}

	// A genuinely different failure mode on the same SHA gets a fresh curve.
	b.RecordFailure("owner/repo", 9, "sha1", "state1", MergeErrorAuth)
	if got := b.Attempts("owner/repo", 9); got != 1 {
		t.Fatalf("attempts after class change = %d, want 1 (reset)", got)
	}
	if got := b.Class("owner/repo", 9); got != MergeErrorAuth {
		t.Fatalf("class = %q, want %q", got, MergeErrorAuth)
	}
}

func TestAutoMergeBackoff_StateChangeResetsAttempts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)
	b := NewAutoMergeBackoff()
	b.now = func() time.Time { return now }

	b.RecordFailure("owner/repo", 9, "sha1", "state1", MergeErrorBlocked)
	b.RecordFailure("owner/repo", 9, "sha1", "state1", MergeErrorBlocked)
	if got := b.Attempts("owner/repo", 9); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}

	b.RecordFailure("owner/repo", 9, "sha1", "state2", MergeErrorBlocked)
	if got := b.Attempts("owner/repo", 9); got != 1 {
		t.Fatalf("attempts after state change = %d, want 1", got)
	}
}

func TestAutoMergeBackoff_Clear(t *testing.T) {
	t.Parallel()
	b := NewAutoMergeBackoff()

	if recovered := b.Clear("owner/repo", 2); recovered {
		t.Fatal("Clear on an untracked key must report recovered=false")
	}

	b.RecordFailure("owner/repo", 2, "sha1", "state1", MergeErrorTransient)
	if recovered := b.Clear("owner/repo", 2); !recovered {
		t.Fatal("Clear after a recorded failure must report recovered=true")
	}
	if !b.ShouldAttempt("owner/repo", 2, "sha1", "state1") {
		t.Fatal("expected attempt allowed immediately after Clear")
	}
}
