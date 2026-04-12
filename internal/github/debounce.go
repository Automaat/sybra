package github

import (
	"sync"
	"time"
)

const maxRetries = 3

// IssueTracker prevents re-dispatching agents for the same PR issue
// within a cooldown period and caps total retries.
type IssueTracker struct {
	mu       sync.Mutex
	handled  map[string]time.Time
	retries  map[string]int
	cooldown time.Duration
	now      func() time.Time // injectable for testing
}

// NewIssueTracker creates a tracker with the given cooldown duration.
func NewIssueTracker(cooldown time.Duration) *IssueTracker {
	return &IssueTracker{
		handled:  make(map[string]time.Time),
		retries:  make(map[string]int),
		cooldown: cooldown,
		now:      time.Now,
	}
}

func issueKey(taskID string, kind PRIssueKind) string {
	return taskID + ":" + string(kind)
}

// ShouldHandle returns true if this issue hasn't been handled within the
// cooldown and hasn't exceeded max retries.
func (t *IssueTracker) ShouldHandle(taskID string, kind PRIssueKind) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := issueKey(taskID, kind)
	if t.retries[key] >= maxRetries {
		return false
	}
	last, ok := t.handled[key]
	if !ok {
		return true
	}
	return t.now().Sub(last) >= t.cooldown
}

// MarkHandled records that an agent was spawned for this issue.
func (t *IssueTracker) MarkHandled(taskID string, kind PRIssueKind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := issueKey(taskID, kind)
	t.handled[key] = t.now()
	t.retries[key]++
}

// ClearCooldown removes the cooldown for a task+issue so the next poll
// can retry immediately. The retry counter is preserved.
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
}

// Retries returns the current retry count for a task+issue.
func (t *IssueTracker) Retries(taskID string, kind PRIssueKind) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retries[issueKey(taskID, kind)]
}

// Cleanup removes entries older than 2x cooldown.
func (t *IssueTracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-2 * t.cooldown)
	for k, v := range t.handled {
		if v.Before(cutoff) {
			delete(t.handled, k)
			delete(t.retries, k)
		}
	}
}
