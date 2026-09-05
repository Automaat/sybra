package workflow

import (
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/errclass"
)

// These predicate-shaped helpers exist only to keep the pre-#3162 literal
// pinning tests readable. Production callers classify once and switch on the
// four-way result; none use these adapters.
func looksLikeGitHubRateLimit(output string) bool {
	return errclass.Classify(output, errclass.WorkflowProseRetryBiased) == errclass.RateLimited
}

func looksLikeTransientGitHub(output string) bool {
	return errclass.Classify(output, errclass.WorkflowProseRetryBiased) == errclass.Transient
}

func looksLikeAuthFailure(output string) bool {
	return errclass.Classify(output, errclass.WorkflowProseRetryBiased) == errclass.Auth
}

// TestShouldRetryVerifyCommitsGitError_PinsLiterals pins the twelve bad-ref
// needles this predicate carried before it moved onto errclass. It had no test
// of any kind, so the whole list was free to drift from internal/project's
// byte-identical copy.
func TestShouldRetryVerifyCommitsGitError_PinsLiterals(t *testing.T) {
	for _, msg := range []string{
		"bad object HEAD",
		"fatal: bad object abc123",
		"not a valid object name",
		"invalid object",
		"fatal: Invalid revision range origin/main...HEAD",
		"missing object",
		"unable to read sha1 file",
		"error: object file .git/objects/ab/cdef is empty",
		"loose object",
		"unknown revision",
		"fatal: ambiguous argument 'origin/main'",
		"reference broken",
	} {
		if !shouldRetryVerifyCommitsGitError(errors.New(msg), nil) {
			t.Errorf("shouldRetryVerifyCommitsGitError(%q) = false, want true", msg)
		}
	}
	if !shouldRetryVerifyCommitsGitError(errors.New("exit 128"), []byte("fatal: bad object HEAD")) {
		t.Error("the git output should be read alongside the error")
	}
	for _, msg := range []string{"connection refused", "Bad credentials"} {
		if shouldRetryVerifyCommitsGitError(errors.New(msg), nil) {
			t.Errorf("shouldRetryVerifyCommitsGitError(%q) = true, want false", msg)
		}
	}
	if shouldRetryVerifyCommitsGitError(nil, []byte("fatal: bad object HEAD")) {
		t.Error("a nil error should never retry")
	}
}

// TestLooksLikeTransientGitHub_PinsLiterals pins the reachability phrases,
// including the bare "timed out" and "tls:" this site keeps locally because it
// reads agent prose rather than a transport error.
func TestLooksLikeTransientGitHub_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{out: "connection refused", want: true},
		{out: "connection reset by peer", want: true},
		{out: "could not resolve host: github.com", want: true},
		{out: "no such host", want: true},
		{out: "no route to host", want: true},
		{out: "network is unreachable", want: true},
		{out: "temporary failure in name resolution", want: true},
		{out: "read tcp: i/o timeout", want: true},
		{out: "the request timed out", want: true},
		{out: "context deadline exceeded", want: true},
		{out: "net/http: TLS handshake timeout", want: true},
		{out: "tls: bad record MAC", want: true},
		{out: "HTTP 502 Bad Gateway", want: true},
		{out: "503 Service Unavailable", want: true},

		{out: "gh: Bad credentials", want: false},
		{out: "API rate limit exceeded", want: false},
		{out: "", want: false},
	} {
		t.Run(tc.out, func(t *testing.T) {
			if got := looksLikeTransientGitHub(tc.out); got != tc.want {
				t.Fatalf("looksLikeTransientGitHub(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestLooksLikeAuthFailure_PinsLiterals pins the credential phrases, including
// the broad "gh auth" this site keeps locally because an echoed command is its
// usual signal.
func TestLooksLikeAuthFailure_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{out: "gh: Bad credentials", want: true},
		{out: "Authentication failed for 'https://github.com/o/r.git/'", want: true},
		{out: "failed to log in", want: true},
		{out: "run: gh auth status", want: true},
		{out: "GH_TOKEN is invalid", want: true},
		{out: "GITHUB_TOKEN is invalid", want: true},
		{out: "the token has expired", want: true},
		{out: "could not read Username for 'https://github.com'", want: true},
		{out: "remote: 401 Unauthorized", want: true},

		{out: "connection refused", want: false},
		{out: "API rate limit exceeded", want: false},
		{out: "", want: false},
	} {
		t.Run(tc.out, func(t *testing.T) {
			if got := looksLikeAuthFailure(tc.out); got != tc.want {
				t.Fatalf("looksLikeAuthFailure(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestLooksLikeGitHubRateLimit_PinsLiterals pins that this site still requires
// a GitHub-shaped corroborator alongside the rate-limit phrase.
func TestLooksLikeGitHubRateLimit_PinsLiterals(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{out: "github: API rate limit exceeded", want: true},
		{out: "graphql: rate limit exceeded", want: true},
		{out: "gh api: secondary rate limit", want: true},
		{out: "GitHub GraphQL rate limit exhausted; I will wait for reset.", want: true},

		{out: "the build rate limit exceeded our budget", want: false},
		{out: "connection refused", want: false},
		{out: "", want: false},
	} {
		t.Run(tc.out, func(t *testing.T) {
			if got := looksLikeGitHubRateLimit(tc.out); got != tc.want {
				t.Fatalf("looksLikeGitHubRateLimit(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
