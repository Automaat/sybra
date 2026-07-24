package github

import (
	"context"
	"strings"
	"sync"
	"time"
)

// AuthState is an explicit GitHub auth health state. Centralizing these
// (rather than each caller inferring "is gh broken?" from its own error
// strings) is what lets a health check, a metrics gauge, and the circuit
// breaker below agree on one picture of the world. See #2453.
type AuthState string

const (
	// AuthHealthy: the last observed gh call (or token mint) succeeded.
	AuthHealthy AuthState = "healthy"
	// AuthRefreshing: a token mint triggered by an observed auth failure is
	// in flight. Transient by construction — never sticks around long enough
	// to matter to a metrics scrape, but is a distinct state so a snapshot
	// taken mid-refresh doesn't misreport "unavailable".
	AuthRefreshing AuthState = "refreshing"
	// AuthRateLimited: the App API itself (JWT-authed /app or installation
	// token mint) is rate limited — distinct from the REST/GraphQL rate
	// limiting ghGate.notBefore already handles, since it's a different
	// GitHub-side bucket.
	AuthRateLimited AuthState = "rate_limited"
	// AuthMisconfigured: a permanent credential problem (revoked key, App
	// suspended, installation removed) that will not resolve by retrying —
	// needs a human to rotate credentials.
	AuthMisconfigured AuthState = "misconfigured"
	// AuthUnavailable: a transient failure (network blip, GitHub 5xx, no App
	// auth configured and ambient `gh auth login` is broken) that may
	// resolve on its own.
	AuthUnavailable AuthState = "unavailable"
)

// AuthSnapshot is a point-in-time view of GitHub auth health, safe to expose
// to logs, health findings, and metrics — it never carries a token or
// request/response content.
type AuthSnapshot struct {
	State               AuthState
	Since               time.Time
	Reason              string
	ConsecutiveFailures int
	RetryAfter          time.Time
	Transitions         int64
	SuppressedCalls     int64
}

// authCircuitBaseBackoff/authCircuitMaxBackoff bound the circuit breaker's
// backoff between calls while auth is misconfigured or unavailable. Doubling
// from a short base means a genuine blip clears in well under a minute, while
// a sustained outage settles at a low, bounded call rate instead of hammering
// gh every 150ms (ghRequestSpacing) for nothing — see #1516 for what that
// costs in log volume alone.
const (
	authCircuitBaseBackoff = 30 * time.Second
	authCircuitMaxBackoff  = 5 * time.Minute
)

type authHealthTracker struct {
	mu sync.Mutex

	state               AuthState
	since               time.Time
	reason              string
	consecutiveFailures int
	nextAttempt         time.Time
	hadFailure          bool
	transitions         int64
	suppressed          int64

	recovered []func()
}

func newAuthHealthTracker() *authHealthTracker {
	return &authHealthTracker{state: AuthHealthy, since: time.Now()}
}

var authHealth = newAuthHealthTracker()

// resetAuthHealthForTest restores the package-global auth health tracker to
// its zero (healthy, no history) state. Test-only.
func resetAuthHealthForTest() {
	authHealth.mu.Lock()
	authHealth.state = AuthHealthy
	authHealth.since = time.Now()
	authHealth.reason = ""
	authHealth.consecutiveFailures = 0
	authHealth.nextAttempt = time.Time{}
	authHealth.hadFailure = false
	authHealth.transitions = 0
	authHealth.suppressed = 0
	authHealth.recovered = nil
	authHealth.mu.Unlock()
}

// OnAuthRecovered registers a callback fired once each time GitHub auth
// health transitions from a non-healthy state back to healthy. Used by the
// durable issue-filing outbox to replay pending filings immediately instead
// of waiting for its next scheduled attempt — the whole point of a "durable
// health state" is that recovery propagates on its own. Callbacks run
// synchronously on the goroutine that observed recovery and must not block;
// callers that need to do real work should hand off to their own goroutine.
func OnAuthRecovered(f func()) {
	if f == nil {
		return
	}
	authHealth.mu.Lock()
	authHealth.recovered = append(authHealth.recovered, f)
	authHealth.mu.Unlock()
}

// AuthHealthSnapshot returns the current GitHub auth health state.
func AuthHealthSnapshot() AuthSnapshot {
	authHealth.mu.Lock()
	defer authHealth.mu.Unlock()
	return AuthSnapshot{
		State:               authHealth.state,
		Since:               authHealth.since,
		Reason:              authHealth.reason,
		ConsecutiveFailures: authHealth.consecutiveFailures,
		RetryAfter:          authHealth.nextAttempt,
		Transitions:         authHealth.transitions,
		SuppressedCalls:     authHealth.suppressed,
	}
}

// AuthCircuitOpen reports whether callers should skip issuing a gh call
// rather than repeat one doomed to fail the same way. Misconfigured,
// unavailable, and rate-limited all trip the circuit: REST/GraphQL rate
// limiting is throttled separately by ghGate's notBefore wall, but the App
// token-mint endpoint AuthRateLimited tracks is a distinct GitHub-side bucket
// with no other throttle, so it backs off here too rather than let every gh
// call re-mint against the already-limited endpoint (see #1516).
// Refreshing/healthy obviously never suppress calls.
func AuthCircuitOpen() (open bool, retryAfter time.Time) {
	authHealth.mu.Lock()
	defer authHealth.mu.Unlock()
	if authHealth.state != AuthMisconfigured &&
		authHealth.state != AuthUnavailable &&
		authHealth.state != AuthRateLimited {
		return false, time.Time{}
	}
	if time.Now().Before(authHealth.nextAttempt) {
		return true, authHealth.nextAttempt
	}
	return false, authHealth.nextAttempt
}

// RecordSuppressedCall increments the suppressed-call counter. Called by a
// gh invocation chokepoint (ghGate.execute, the monitor package's issue-sink
// execer) exactly when it skips shelling out because AuthCircuitOpen()
// returned true, so the metrics gauge reflects calls actually avoided, not
// just the state that would have avoided them.
func RecordSuppressedCall() {
	authHealth.mu.Lock()
	authHealth.suppressed++
	authHealth.mu.Unlock()
}

// setState is the single place authHealth's state changes. Firing the
// recovered callbacks outside the lock avoids a callback that (transitively)
// reads AuthHealthSnapshot from deadlocking against authHealth.mu.
func (t *authHealthTracker) setState(state AuthState, reason string) {
	t.mu.Lock()
	if state != t.state {
		t.state = state
		t.since = time.Now()
		t.transitions++
	}
	t.reason = reason

	var fireRecovery []func()
	switch state {
	case AuthHealthy:
		t.consecutiveFailures = 0
		t.nextAttempt = time.Time{}
		if t.hadFailure {
			t.hadFailure = false
			fireRecovery = append(fireRecovery, t.recovered...)
		}
	case AuthRefreshing:
		// In-between state: doesn't itself count as a failure or clear one.
	case AuthMisconfigured, AuthUnavailable:
		t.applyFailureBackoffLocked()
	case AuthRateLimited:
		// The App token-mint endpoint is a distinct GitHub-side bucket that
		// ghGate's notBefore wall does not throttle, so back off on the same
		// bounded schedule as the other failure states — otherwise every
		// subsequent gh call re-triggers a force-mint against the
		// already-rate-limited endpoint (see onAuthFailureObserved, #1516).
		t.applyFailureBackoffLocked()
	}
	t.mu.Unlock()

	for _, cb := range fireRecovery {
		cb()
	}
}

// applyFailureBackoffLocked records a failure and advances nextAttempt on the
// bounded exponential schedule shared by every circuit-tripping state. Caller
// must hold t.mu.
func (t *authHealthTracker) applyFailureBackoffLocked() {
	t.hadFailure = true
	t.consecutiveFailures++
	backoff := authCircuitBaseBackoff
	for i := 1; i < t.consecutiveFailures && backoff < authCircuitMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > authCircuitMaxBackoff {
		backoff = authCircuitMaxBackoff
	}
	t.nextAttempt = time.Now().Add(backoff)
}

// isAuthErrorMsg is IsAuthError's message-matching core, factored out so
// ObserveCallResult can classify a combined stdout+stderr blob the same way
// IsAuthError classifies a wrapped error.
func isAuthErrorMsg(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "bad credentials") ||
		strings.Contains(msg, "gh auth login") ||
		strings.Contains(msg, "gh_token environment variable") ||
		strings.Contains(msg, authCircuitOpenMarker)
}

// authCircuitOpenMarker tags the synthetic error returned while the circuit
// is open, so a caller that classifies it via IsAuthError (the durable
// outbox, push-preflight retry, poller circuits) treats a suppressed call the
// same way it treats a real one instead of mistaking it for an unrelated
// failure.
const authCircuitOpenMarker = "github auth circuit open"

// NewAuthCircuitOpenError builds the synthetic error a gh invocation
// chokepoint returns when it suppresses a call instead of shelling out.
// Exported so callers outside this package (internal/monitor's issue-filing
// execer, which shells out directly rather than through ghGate) can report
// the same error shape instead of hand-rolling their own suppressed-call
// message.
func NewAuthCircuitOpenError(retryAfter time.Time) error {
	return &authCircuitOpenError{retryAfter: retryAfter}
}

type authCircuitOpenError struct {
	retryAfter time.Time
}

func (e *authCircuitOpenError) Error() string {
	return authCircuitOpenMarker + ": suppressing gh calls until " + e.retryAfter.UTC().Format(time.RFC3339)
}

// ObserveCallResult updates the shared GitHub auth-health state from the
// outcome of a completed gh invocation, for callers with no caller context to
// propagate. Equivalent to ObserveCallResultCtx(context.Background(), ...).
func ObserveCallResult(out []byte, err error) {
	ObserveCallResultCtx(context.Background(), out, err)
}

// ObserveCallResultCtx is ObserveCallResult's context-aware form. Call sites
// are the package's own request gate (ghGate.execute) and internal/monitor's
// issue-filing execer, which mirrors credential handling here the same way it
// already mirrors ghEnv() — see GHEnv(). Non-auth errors (rate limiting,
// network blips, 4xx unrelated to credentials) are left to their own handling
// and never change auth state.
//
// On a token-specific auth failure this triggers exactly one force-refresh
// attempt when App auth is configured (appTokenSource.refresh's own
// singleflight collapses any concurrent callers into that one mint call —
// see appauth.go). A refresh success clears the circuit; a refresh failure
// (or no App auth to refresh at all) classifies the state as misconfigured
// or unavailable, which is what AuthCircuitOpen then suppresses on. The mint
// itself is detached from ctx's cancellation (see onAuthFailureObserved) so a
// caller's own short-lived deadline never aborts a refresh other concurrent
// callers are waiting on.
func ObserveCallResultCtx(ctx context.Context, out []byte, err error) {
	if err == nil {
		authHealth.setState(AuthHealthy, "")
		return
	}
	msg := err.Error() + " " + string(out)
	if !isAuthErrorMsg(msg) {
		return
	}
	onAuthFailureObserved(ctx, msg)
}

func onAuthFailureObserved(ctx context.Context, reason string) {
	src := currentAppSource()
	if src == nil {
		// No App auth configured — nothing we can force-refresh. An ambient
		// `gh auth login` credential going stale/missing needs a human, not
		// a retry.
		authHealth.setState(AuthUnavailable, reason)
		return
	}
	authHealth.setState(AuthRefreshing, reason)
	// context.WithoutCancel: this refresh runs synchronously on the
	// goroutine that observed the failure and must not inherit that
	// specific call's cancellation/deadline — a short-lived poll context
	// timing out mid-mint must not abort a refresh other concurrent 401s are
	// singleflight-waiting on (see appauth.go's refreshMu).
	if err := ForceRefreshAppTokenEnv(context.WithoutCancel(ctx)); err != nil {
		authHealth.setState(classifyMintError(err), err.Error())
		return
	}
	authHealth.setState(AuthHealthy, "")
}
