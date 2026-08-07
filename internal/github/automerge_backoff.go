package github

import (
	"strconv"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/backoff"
	"github.com/Automaat/sybra/internal/errclass"
)

// MergeErrorClass classifies a failed auto-merge attempt so AutoMergeBackoff
// can size its retry window per failure mode instead of treating every
// error the same.
type MergeErrorClass string

const (
	// MergeErrorAuth is a GitHub authentication/credential failure —
	// retrying on a normal schedule would just repeat the same 401 until the
	// credential is fixed out-of-band.
	MergeErrorAuth MergeErrorClass = "auth"
	// MergeErrorRateLimit is a primary/secondary rate-limit response. The
	// shared request gate (ghGate) already paces individual gh calls; this
	// class holds the merge-specific caller off even longer before spending
	// another attempt against the same budget.
	MergeErrorRateLimit MergeErrorClass = "rate_limit"
	// MergeErrorTransient is a network/infrastructure blip (timeout,
	// connection reset, 502/503/504) unrelated to PR state — worth a
	// relatively quick retry.
	MergeErrorTransient MergeErrorClass = "transient"
	// MergeErrorBlocked is a GitHub-reported blocking condition tied to
	// review/branch-protection state Sybra's own readiness gate does not
	// fully model (e.g. an extra required approval, a policy that requires
	// --auto). Only a state change resolves it, so it gets the longest
	// backoff.
	MergeErrorBlocked MergeErrorClass = "blocked"
	// MergeErrorUnknown is any other failure. Backed off the same as
	// MergeErrorBlocked out of caution.
	MergeErrorUnknown MergeErrorClass = "unknown"
)

// ClassifyMergeError buckets a failed MergePR/MergePRViaREST/EnableAutoMerge
// error so AutoMergeBackoff can size its retry window per failure mode.
// Uses the auth-first policy because the legacy caller checked auth, then rate
// limit, then transient evidence before its merge-state bucket.
func ClassifyMergeError(err error) MergeErrorClass {
	if err == nil {
		return ""
	}
	switch errclass.ClassifyErr(err, errclass.GitHubCircuitEscalationBiased) {
	case errclass.Auth:
		return MergeErrorAuth
	case errclass.RateLimited:
		return MergeErrorRateLimit
	case errclass.Transient:
		return MergeErrorTransient
	case errclass.Permanent:
		return MergeErrorBlocked
	default:
		return MergeErrorUnknown
	}
}

// autoMergeBackoffWindow maps a MergeErrorClass to its base retry delay and
// the ceiling its exponential growth saturates at. A transient network blip
// retries far sooner than a state genuinely stuck behind a review/branch-
// protection check that only a human or a new push can resolve.
var autoMergeBackoffWindow = map[MergeErrorClass]struct{ base, max time.Duration }{
	MergeErrorAuth:      {5 * time.Minute, time.Hour},
	MergeErrorRateLimit: {5 * time.Minute, time.Hour},
	MergeErrorTransient: {30 * time.Second, 10 * time.Minute},
	MergeErrorBlocked:   {2 * time.Minute, 2 * time.Hour},
	MergeErrorUnknown:   {2 * time.Minute, 2 * time.Hour},
}

func mergeBackoffWindow(class MergeErrorClass) (base, ceiling time.Duration) {
	if w, ok := autoMergeBackoffWindow[class]; ok {
		return w.base, w.max
	}
	return 2 * time.Minute, 2 * time.Hour
}

type autoMergeBackoffEntry struct {
	headSHA  string
	stateSig string
	class    MergeErrorClass
	attempts int
	nextTry  time.Time
}

// AutoMergeBackoff persists a retry fingerprint per repository, PR, head
// SHA, and error class so a repeatedly failing auto-merge/arm attempt backs
// off exponentially instead of re-attempting — and re-logging — on every
// poll tick against unchanged PR state (#2450: 1,250 auto-merge.failed
// events logged in seven days against static state). A head-SHA change (new
// push) or an error-class change always reprobes immediately: either means
// the prior backoff schedule no longer describes the problem being retried.
type AutoMergeBackoff struct {
	mu      sync.Mutex
	entries map[string]autoMergeBackoffEntry
	now     func() time.Time // injectable for testing
}

// NewAutoMergeBackoff creates an empty backoff tracker.
func NewAutoMergeBackoff() *AutoMergeBackoff {
	return &AutoMergeBackoff{entries: make(map[string]autoMergeBackoffEntry), now: time.Now}
}

// autoMergeBackoffKey mirrors review.prRefCacheKey's "repo#number" format
// without importing across the package boundary.
func autoMergeBackoffKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// ShouldAttempt reports whether a merge/arm attempt against repo#number at
// headSHA should proceed now. True on the first sighting of this head SHA or
// stateSig (nothing to back off from yet) and once the backoff window from the
// last recorded failure has elapsed.
func (b *AutoMergeBackoff) ShouldAttempt(repo string, number int, headSHA, stateSig string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[autoMergeBackoffKey(repo, number)]
	if !ok || entry.headSHA != headSHA || entry.stateSig != stateSig {
		return true
	}
	return !b.now().Before(entry.nextTry)
}

// RecordFailure records a failed attempt and computes the next eligible retry
// time. A head-SHA, stateSig, or error-class change resets the attempt counter
// — a new push, changed PR/check state, or a genuinely different failure mode
// gets its own backoff curve rather than inheriting the prior problem's
// position on it.
// atCeiling reports whether this failure's computed delay has saturated at
// its class's maximum, i.e. this PR has been failing long enough that
// backoff can no longer grow — a signal worth surfacing (see
// metrics.AutoMergeAttempt "terminal") without escalating the task, since a
// green PR that simply hasn't merged is never itself a failure.
func (b *AutoMergeBackoff) RecordFailure(repo string, number int, headSHA, stateSig string, class MergeErrorClass) (atCeiling bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := autoMergeBackoffKey(repo, number)
	entry, ok := b.entries[key]
	if !ok || entry.headSHA != headSHA || entry.stateSig != stateSig || entry.class != class {
		entry = autoMergeBackoffEntry{headSHA: headSHA, stateSig: stateSig, class: class}
	}
	entry.attempts++
	base, ceiling := mergeBackoffWindow(class)
	computed := backoff.ForAttempt(entry.attempts, base, ceiling)
	entry.nextTry = b.now().Add(computed.Delay)
	b.entries[key] = entry
	return computed.AtCeiling
}

// Clear drops backoff state for repo#number, reporting whether an entry
// existed — i.e. this PR had a failure backed off before recovering — so
// the caller can distinguish a routine first-time success from a genuine
// recovery out of a suppressed-retry state.
func (b *AutoMergeBackoff) Clear(repo string, number int) (recovered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := autoMergeBackoffKey(repo, number)
	_, ok := b.entries[key]
	delete(b.entries, key)
	return ok
}

// Attempts returns the current attempt count recorded for repo#number.
// Exposed for tests.
func (b *AutoMergeBackoff) Attempts(repo string, number int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entries[autoMergeBackoffKey(repo, number)].attempts
}

// Class returns the error class currently tracked for repo#number, or ""
// when nothing is tracked.
func (b *AutoMergeBackoff) Class(repo string, number int) MergeErrorClass {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entries[autoMergeBackoffKey(repo, number)].class
}
