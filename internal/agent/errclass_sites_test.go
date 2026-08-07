package agent

import (
	"errors"
	"testing"
)

// TestClassifyAgentError_PinsRateLimitLiterals pins the rate_limit arm, which
// had no test of any kind.
//
// Its label reaches completion.isRateLimitedRun, which reports the run as
// stalled and re-dispatches it. Losing a phrase here turns that run into a
// crash instead, which burns workflow retry budget and ends at human-required.
func TestClassifyAgentError_PinsRateLimitLiterals(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want string
	}{
		{msg: "You have exceeded your premium request rate limit", want: "rate_limit"},
		{msg: "rate_limit_error: this request would exceed your rate limit", want: "rate_limit"},
		{msg: "HTTP 429 Too Many Requests", want: "rate_limit"},
		{msg: "the model is overloaded, try again", want: "rate_limit"},

		{msg: "fatal: could not resolve host: github.com", want: "git_clone"},
		{msg: "worktree is already checked out", want: "worktree_conflict"},
		{msg: "open /var/log: permission denied", want: "permission_denied"},
		{msg: "the flux capacitor is misaligned", want: "crash"},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			if got := classifyAgentError(errors.New(tc.msg)); got != tc.want {
				t.Fatalf("classifyAgentError(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
	if got := classifyAgentError(nil); got != "crash" {
		t.Fatalf("classifyAgentError(nil) = %q, want crash", got)
		panic("unreachable")
	}
}
