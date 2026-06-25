package github

import (
	"sync"
	"time"
)

const maxRetries = 3

// MaxRetries is the exported retry cap for callers that need to reference it
// in escalation messages or tests.
const MaxRetries = maxRetries

// IssueTracker prevents re-dispatching agents for the same PR issue
// within a cooldown period and caps total retries.
type IssueTracker struct {
	mu       sync.Mutex
	handled  map[string]time.Time
	retries  map[string]int
	lastSHA  map[string]string
	cooldown time.Duration
	now      func() time.Time // injectable for testing
}

// NewIssueTracker creates a tracker with the given cooldown duration.
func NewIssueTracker(cooldown time.Duration) *IssueTracker {
	return &IssueTracker{
		handled:  make(map[string]time.Time),
		retries:  make(map[string]int),
		lastSHA:  make(map[string]string),
		cooldown: cooldown,
		now:      time.Now,
	}
}

func issueKey(taskID string, kind PRIssueKind) string {
	return taskID + ":" + string(kind)
}

// ShouldHandle returns true if this issue should be retried.
// Blocks when: max retries reached, same SHA as last attempt (no new commit),
// or within cooldown window (first attempt or SHA unknown).
func (t *IssueTracker) ShouldHandle(taskID string, kind PRIssueKind, sha string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := issueKey(taskID, kind)
	if t.retries[key] >= maxRetries {
		return false
	}
	// If SHA is known and unchanged, the fix attempt ran against this exact
	// commit with no new push since — skip until a new commit arrives.
	if sha != "" && t.lastSHA[key] == sha {
		return false
	}
	last, ok := t.handled[key]
	if !ok {
		return true
	}
	return t.now().Sub(last) >= t.cooldown
}

// MarkHandled records that an agent was spawned for this issue.
func (t *IssueTracker) MarkHandled(taskID string, kind PRIssueKind, sha string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := issueKey(taskID, kind)
	t.handled[key] = t.now()
	t.retries[key]++
	if sha != "" {
		t.lastSHA[key] = sha
	}
}

// ClearCooldown removes the time-based cooldown for a task+issue so the next
// poll can retry immediately if the SHA has changed. The retry counter and
// last-attempt SHA are preserved.
func (t *IssueTracker) ClearCooldown(taskID string, kind PRIssueKind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.handled, issueKey(taskID, kind))
}

// Clear removes all tracking for a task+issue (call when issue resolves).
func (t *IssueTracker) Clear(taskID string, kind PRIssueKind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := issueKey(taskID, kind)
	delete(t.handled, key)
	delete(t.retries, key)
	delete(t.lastSHA, key)
}

// Retries returns the current retry count for a task+issue.
func (t *IssueTracker) Retries(taskID string, kind PRIssueKind) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retries[issueKey(taskID, kind)]
}

// AtCap reports whether the task+issue has exhausted its retry budget.
func (t *IssueTracker) AtCap(taskID string, kind PRIssueKind) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retries[issueKey(taskID, kind)] >= maxRetries
}

// Cleanup removes entries older than 2x cooldown. Entries that have hit the
// retry cap are kept so the escalation path in the caller survives long gaps
// between flaky CI runs — without this, Cleanup would reset the counter and
// the cap would never fire.
func (t *IssueTracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-2 * t.cooldown)
	for k, v := range t.handled {
		if v.Before(cutoff) {
			if t.retries[k] >= maxRetries {
				// At cap: keep retries/lastSHA so the caller can escalate;
				// only drop the time-based handled entry (no more cooldown).
				delete(t.handled, k)
				continue
			}
			delete(t.handled, k)
			delete(t.retries, k)
			delete(t.lastSHA, k)
		}
	}
}
