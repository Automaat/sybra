package review

import (
	"testing"

	"github.com/Automaat/sybra/internal/github"
)

// TestPRSnapshotStore_ClearReadyPreservesBackoff locks in the reason
// ready-cache and poll-backoff state were merged into one entry per key
// rather than kept as two maps that could drift apart: evicting the
// ready-to-merge snapshot (as handleAutoMerge does the moment it acts on a
// PR) must never reset the independent poll-backoff counters.
func TestPRSnapshotStore_ClearReadyPreservesBackoff(t *testing.T) {
	var s PRSnapshotStore
	pr := github.PullRequest{Number: 1, HeadSHA: "sha1"}
	s.SetReady("k", "sha1", "2026-01-01T00:00:00Z", pr)
	s.NoteResult("k", "sha1", "2026-01-01T00:00:00Z", 8)
	s.NoteResult("k", "sha1", "2026-01-01T00:00:00Z", 8) // second stable observation

	_, _, skipBefore, streakBefore := s.Backoff("k")
	if skipBefore == 0 || streakBefore == 0 {
		t.Fatalf("precondition: want nonzero backoff state, got skip=%d streak=%d", skipBefore, streakBefore)
	}

	s.ClearReady("k")

	if _, ok := s.Ready("k"); ok {
		t.Fatal("Ready() = true after ClearReady, want false")
	}
	_, _, skipAfter, streakAfter := s.Backoff("k")
	if skipAfter != skipBefore || streakAfter != streakBefore {
		t.Fatalf("backoff state disturbed by ClearReady: before skip=%d streak=%d, after skip=%d streak=%d",
			skipBefore, streakBefore, skipAfter, streakAfter)
	}
}

// TestPRSnapshotStore_NoteResultResetsBackoffOnChange locks in that a
// head-SHA or updatedAt change resets the backoff counters, and an unchanged
// pair advances them — the single invalidation rule both the ready-cache and
// poll-backoff halves of the store now share.
func TestPRSnapshotStore_NoteResultResetsBackoffOnChange(t *testing.T) {
	var s PRSnapshotStore
	s.NoteResult("k", "sha1", "t1", 8)
	s.NoteResult("k", "sha1", "t1", 8)
	if _, _, skip, streak := s.Backoff("k"); skip == 0 || streak == 0 {
		t.Fatalf("want nonzero backoff after two stable observations, got skip=%d streak=%d", skip, streak)
	}

	s.NoteResult("k", "sha2", "t2", 8)
	if _, _, skip, streak := s.Backoff("k"); skip != 0 || streak != 0 {
		t.Fatalf("want backoff reset after a head-SHA change, got skip=%d streak=%d", skip, streak)
	}
}

func TestPRSnapshotStore_Prune(t *testing.T) {
	var s PRSnapshotStore
	s.NoteResult("keep", "sha1", "t1", 8)
	s.NoteResult("drop", "sha1", "t1", 8)

	s.Prune(map[string]struct{}{"keep": {}})

	if got := s.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
	if headSHA, _, _, _ := s.Backoff("keep"); headSHA != "sha1" {
		t.Fatalf("kept entry's headSHA = %q, want sha1 (unaffected by prune)", headSHA)
	}
	if headSHA, _, _, _ := s.Backoff("drop"); headSHA != "" {
		t.Fatalf("dropped entry's headSHA = %q, want empty (pruned)", headSHA)
	}
}
