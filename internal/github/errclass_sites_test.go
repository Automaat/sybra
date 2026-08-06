package github

import (
	"errors"
	"testing"
)

// TestIsTransientGHError_PinsLiterals pins the literal set isTransientGHError
// matched before it moved onto errclass. StreamPhrases in particular had no
// test outside the new package, so its retry was unguarded.
func TestIsTransientGHError_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{out: "gh: HTTP 502", want: true},
		{out: "gh: HTTP 503", want: true},
		{out: "gh: HTTP 504", want: true},
		{out: "operation timed out", want: true},
		{out: "read tcp: i/o timeout", want: true},
		{out: "context deadline exceeded", want: true},
		{out: "deadline exceeded", want: true},
		{out: "connection reset by peer", want: true},
		{out: "connect: connection refused", want: true},
		{out: "http2: stream error", want: true},
		{out: "unexpected EOF", want: true},
		{out: "net/http: TLS handshake timeout", want: true},
		{out: "tls: first record does not look like a TLS handshake", want: true},

		{out: "gh: HTTP 500", want: false},
		{out: "gh: HTTP 404", want: false},
		{out: "gh: Bad credentials", want: false},
		{out: "", want: false},
	} {
		t.Run(tc.out, func(t *testing.T) {
			got := isTransientGHError([]byte(tc.out), errors.New(tc.out))
			if got != tc.want {
				t.Fatalf("isTransientGHError(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestIsRateLimitedMessage_PinsLiterals pins the four phrases this predicate
// recognized before the move.
func TestIsRateLimitedMessage_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{msg: "You have exceeded a secondary rate limit", want: true},
		{msg: "API rate limit exceeded for user ID 1", want: true},
		{msg: "rate limit exceeded", want: true},
		{msg: rateLimitWallMarker, want: true},
		{msg: "API rate limit exceeded", want: true},
		{msg: "SECONDARY RATE LIMIT", want: true},

		{msg: "connection refused", want: false},
		{msg: "Bad credentials", want: false},
		{msg: "", want: false},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			if got := isRateLimitedMessage(tc.msg); got != tc.want {
				t.Fatalf("isRateLimitedMessage(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestIsAuthErrorMsg_PinsLiterals pins the credential phrases, including the
// two that stay local to this package: the narrow "gh auth login" and the
// GH_TOKEN hint.
func TestIsAuthErrorMsg_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{msg: "gh: HTTP 401", want: true},
		{msg: "gh: Bad credentials", want: true},
		{msg: "To get started with GitHub CLI, please run: gh auth login", want: true},
		{msg: "the GH_TOKEN environment variable is not set", want: true},
		{msg: authCircuitOpenMarker, want: true},

		// A missing-scope hint never self-heals; treating it as an auth error
		// would hold the shared circuit open across every gh call.
		{msg: "gh: Your token has not been granted the required scopes. To request it, run: gh auth refresh -s read:org", want: false},
		{msg: "fatal: Authentication failed for 'https://github.com/o/r.git/'", want: false},
		{msg: "remote: 401 Unauthorized", want: false},
		{msg: "connection refused", want: false},
		{msg: "", want: false},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			if got := isAuthErrorMsg(tc.msg); got != tc.want {
				t.Fatalf("isAuthErrorMsg(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestClassifyMergeError_BlockedStaysBlocked pins the backoff-collapse defect:
// a blocked merge whose text mentions a timed-out check must not read as
// transient, or its ceiling drops from two hours to ten minutes.
func TestClassifyMergeError_BlockedStaysBlocked(t *testing.T) {
	for _, msg := range []string{
		`Pull request is not mergeable: required status check "e2e" timed out`,
		"base branch policy prohibits the merge; the required status check timed out",
		"blocked by a policy: waiting for status checks; the connection timed out earlier but has recovered",
		"gh: not mergeable: waiting for status checks",
	} {
		if got := ClassifyMergeError(errors.New(msg)); got != MergeErrorBlocked {
			t.Errorf("ClassifyMergeError(%q) = %q, want %q", msg, got, MergeErrorBlocked)
		}
	}
}
