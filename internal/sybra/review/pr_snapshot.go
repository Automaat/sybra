package review

import (
	"time"

	"github.com/Automaat/sybra/internal/github"
)

// prSnapshot is one linked PR's cross-poll-cycle bookkeeping, keyed by
// "repo#number" (prRefCacheKey). It replaces two previously separate caches —
// readyPRCache and prPollState — that each tracked their own copy of the same
// head-SHA/updatedAt pair for a different purpose (a ready-to-merge fetch
// shortcut vs. poll-backoff bookkeeping) but were always written from the
// same fetch result in lockstep, so the two copies could never actually
// diverge. One pair now backs both.
type prSnapshot struct {
	headSHA   string
	updatedAt string
	// ready holds the last confirmed ready-to-merge PR body at headSHA/
	// updatedAt, or nil if the PR wasn't ready (or hasn't been fetched) at
	// that snapshot. Independent of the backoff fields below: evicting it
	// (once handleAutoMerge acts on the PR) must not reset poll backoff.
	ready        *github.PullRequest
	stableStreak int
	skipTicks    int
	// statusChangedAt is the linked task's StatusChangedAt as of the last tick
	// that selected this PR. Backoff is keyed on the PR, so without this a task
	// re-entering a fixable lane would keep serving out a streak the PR earned
	// while the task was parked and unfixable.
	statusChangedAt time.Time
}

// PRSnapshotStore is the single owner of per-PR ("repo#number") poll-cycle
// state for the known-linked-PR poll loop.
type PRSnapshotStore struct {
	entries map[string]prSnapshot
}

func (s *PRSnapshotStore) get(key string) prSnapshot {
	return s.entries[key]
}

func (s *PRSnapshotStore) set(key string, e prSnapshot) {
	if s.entries == nil {
		s.entries = make(map[string]prSnapshot)
	}
	s.entries[key] = e
}

// Ready returns the cached ready-to-merge PR for key, if any.
func (s *PRSnapshotStore) Ready(key string) (github.PullRequest, bool) {
	e, ok := s.entries[key]
	if !ok || e.ready == nil {
		return github.PullRequest{}, false
	}
	return *e.ready, true
}

// SetReady records pr as key's confirmed ready-to-merge snapshot at
// headSHA/updatedAt.
func (s *PRSnapshotStore) SetReady(key, headSHA, updatedAt string, pr github.PullRequest) {
	e := s.get(key)
	e.headSHA = headSHA
	e.updatedAt = updatedAt
	prCopy := pr
	e.ready = &prCopy
	s.set(key, e)
}

// ClearReady drops key's cached ready-to-merge snapshot (if any) without
// disturbing its poll-backoff bookkeeping.
func (s *PRSnapshotStore) ClearReady(key string) {
	e, ok := s.entries[key]
	if !ok || e.ready == nil {
		return
	}
	e.ready = nil
	s.set(key, e)
}

// Backoff returns key's last-observed head-SHA/updatedAt pair and current
// skip-tick/stable-streak counters.
func (s *PRSnapshotStore) Backoff(key string) (headSHA, updatedAt string, skipTicks, stableStreak int) {
	e := s.entries[key]
	return e.headSHA, e.updatedAt, e.skipTicks, e.stableStreak
}

// TaskStatusAdvancedSince reports whether the linked task changed status after
// the last tick that selected key. A task moving back into a fixable lane must
// be re-probed immediately rather than waiting out the PR's stable streak.
func (s *PRSnapshotStore) TaskStatusAdvancedSince(key string, statusChangedAt time.Time) bool {
	e, ok := s.entries[key]
	if !ok {
		return true
	}
	return statusChangedAt.After(e.statusChangedAt)
}

// NoteTaskStatus stamps the linked task's StatusChangedAt onto key.
func (s *PRSnapshotStore) NoteTaskStatus(key string, statusChangedAt time.Time) {
	e := s.get(key)
	e.statusChangedAt = statusChangedAt
	s.set(key, e)
}

// DecrementSkipTicks consumes one skip-tick for key, leaving every other
// field untouched.
func (s *PRSnapshotStore) DecrementSkipTicks(key string) {
	e := s.get(key)
	e.skipTicks--
	s.set(key, e)
}

// ResetBackoff clears key's skip-tick/stable-streak counters — used the
// moment a probe observes the PR's state has actually changed.
func (s *PRSnapshotStore) ResetBackoff(key string) {
	e := s.get(key)
	e.skipTicks = 0
	e.stableStreak = 0
	s.set(key, e)
}

// NoteResult records a fresh fetch result for key: a head-SHA/updatedAt
// change resets the backoff counters to zero (a real push always reprobes
// immediately next cycle); an unchanged pair advances the stable streak and
// recomputes the exponential skip-tick backoff.
func (s *PRSnapshotStore) NoteResult(key, headSHA, updatedAt string, maxTicks int) {
	e, ok := s.entries[key]
	if !ok || e.headSHA != headSHA || e.updatedAt != updatedAt {
		e.headSHA = headSHA
		e.updatedAt = updatedAt
		e.stableStreak = 0
		e.skipTicks = 0
		s.set(key, e)
		return
	}
	e.stableStreak++
	e.skipTicks = expBackoff(e.stableStreak, maxTicks)
	s.set(key, e)
}

// Prune drops every entry whose key is not in seen — PRs no longer linked to
// a monitored task.
func (s *PRSnapshotStore) Prune(seen map[string]struct{}) {
	for key := range s.entries {
		if _, ok := seen[key]; !ok {
			delete(s.entries, key)
		}
	}
}

// Len reports how many PRs the store currently tracks. Test-only helper.
func (s *PRSnapshotStore) Len() int {
	return len(s.entries)
}
