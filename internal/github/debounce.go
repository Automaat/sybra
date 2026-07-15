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
// When sig is unchanged it caps at MaxRetries and returns DispatchExhausted —
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

// Cleanup drops the time-based cooldown record for entries older than 2x
// cooldown, letting a subsequent poll dispatch immediately once a genuinely
// new commit or feedback signature shows up — but it never touches
// retries/lastSHA/sigs. Those three are the durable per-commit memory: an
// unchanged head SHA must keep skipping dispatch (and an at-cap signature
// must keep escalating) no matter how long the PR sits idle between polls.
// A prior version wiped retries/lastSHA/sigs here once retries were below
// the cap, which meant a long-lived PR whose head never changed would forget
// it had already been handled and re-dispatch a fresh pr-fix agent forever
// (#1528 — 42 runs against one static head SHA over 10 days).
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
