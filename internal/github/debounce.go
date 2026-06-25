package github

import (
	"sync"
	"time"
)

const maxRetries = 3

// MaxRetries is the per-issue retry budget the monitor allows before escalating
// to a human. Exported for callers that reference it in escalation messages or
// tests.
const MaxRetries = maxRetries

// DispatchDecision is the verdict of Decide for one detected PR issue.
type DispatchDecision int

const (
	// DispatchSkip: do nothing this cycle (cooldown, no new commit, or the
	// exact attempt already ran).
	DispatchSkip DispatchDecision = iota
	// DispatchHandle: dispatch a fix agent now.
	DispatchHandle
	// DispatchExhausted: the retry budget for the current feedback signature is
	// spent — escalate to a human instead of looping.
	DispatchExhausted
)

// IssueTracker prevents re-dispatching agents for the same PR issue
// within a cooldown period and caps total retries.
type IssueTracker struct {
	mu       sync.Mutex
	handled  map[string]time.Time
	retries  map[string]int
	lastSHA  map[string]string
	sigs     map[string]string
	cooldown time.Duration
	now      func() time.Time // injectable for testing
}

// NewIssueTracker creates a tracker with the given cooldown duration.
func NewIssueTracker(cooldown time.Duration) *IssueTracker {
	return &IssueTracker{
		handled:  make(map[string]time.Time),
		retries:  make(map[string]int),
		lastSHA:  make(map[string]string),
		sigs:     make(map[string]string),
		cooldown: cooldown,
		now:      time.Now,
	}
}

func issueKey(taskID string, kind PRIssueKind) string {
	return taskID + ":" + string(kind)
}

// Decide is the signature-aware dispatch gate. When sig differs from the last
// one recorded for this issue, the feedback genuinely changed since the last
// attempt, so the retry budget resets (new feedback deserves a fresh response).
// When sig is unchanged it caps at MaxFixRetries and returns DispatchExhausted —
// the caller escalates to a human instead of looping forever. A "" sig disables
// signature gating (legacy SHA-only behavior), used for issue kinds without a
// feedback fingerprint (conflict, ci_failure, ready_to_merge).
func (t *IssueTracker) Decide(taskID string, kind PRIssueKind, sha, sig string) DispatchDecision {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := issueKey(taskID, kind)

	// New feedback since the last attempt: reset the budget and the per-commit
	// gate so the agent can respond to it immediately.
	if sig != "" && t.sigs[key] != sig {
		t.retries[key] = 0
		delete(t.handled, key)
		delete(t.lastSHA, key)
		t.sigs[key] = sig
	}

	if t.retries[key] >= maxRetries {
		return DispatchExhausted
	}
	// If SHA is known and unchanged, the fix attempt ran against this exact
	// commit with no new push since — skip until a new commit arrives.
	if sha != "" && t.lastSHA[key] == sha {
		return DispatchSkip
	}
	last, ok := t.handled[key]
	if !ok {
		return DispatchHandle
	}
	if t.now().Sub(last) >= t.cooldown {
		return DispatchHandle
	}
	return DispatchSkip
}

// ShouldHandle returns true if this issue should be retried. Equivalent to
// Decide with no feedback signature (SHA-only gating).
func (t *IssueTracker) ShouldHandle(taskID string, kind PRIssueKind, sha string) bool {
	return t.Decide(taskID, kind, sha, "") == DispatchHandle
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
	delete(t.sigs, key)
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
			delete(t.sigs, k)
		}
	}
}
