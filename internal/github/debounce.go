package github

import (
	"sync"
	"time"
)

// IssueTracker prevents re-dispatching agents for the same PR issue
// within a cooldown period.
type IssueTracker struct {
	mu       sync.Mutex
	handled  map[string]time.Time
	cooldown time.Duration
	now      func() time.Time // injectable for testing
}

// NewIssueTracker creates a tracker with the given cooldown duration.
func NewIssueTracker(cooldown time.Duration) *IssueTracker {
	return &IssueTracker{
		handled:  make(map[string]time.Time),
		cooldown: cooldown,
		now:      time.Now,
	}
}

func issueKey(taskID string, kind PRIssueKind) string {
	return taskID + ":" + string(kind)
}

// ShouldHandle returns true if this issue hasn't been handled within the cooldown.
func (t *IssueTracker) ShouldHandle(taskID string, kind PRIssueKind) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.handled[issueKey(taskID, kind)]
	if !ok {
		return true
	}
	return t.now().Sub(last) >= t.cooldown
}

// MarkHandled records that an agent was spawned for this issue.
func (t *IssueTracker) MarkHandled(taskID string, kind PRIssueKind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handled[issueKey(taskID, kind)] = t.now()
}

// Clear removes tracking for a task+issue (call when issue resolves).
func (t *IssueTracker) Clear(taskID string, kind PRIssueKind) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.handled, issueKey(taskID, kind))
}

// Cleanup removes entries older than 2x cooldown.
func (t *IssueTracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := t.now().Add(-2 * t.cooldown)
	for k, v := range t.handled {
		if v.Before(cutoff) {
			delete(t.handled, k)
		}
	}
}
