// Package errclass holds every phrase table used to decide whether an error
// is transient, rate limited, or an auth failure.
//
// The tables used to live in a dozen packages, where they drifted apart
// unseen. The twelve bad-ref needles existed byte-identically in
// internal/project and internal/workflow. internal/monitor knew two of the
// four rate-limit phrases internal/github knew. Nothing made a reader of one
// table aware of the others.
//
// This package co-locates them and does not merge them. Each caller keeps the
// exact set it had, because the sets differ for good reasons: IsTransientError
// gates poller escalation, where under-reporting transience escalates the
// whole board; IsTransientNetworkError becomes worktree.ErrTransientFetch,
// which callers must never escalate, so over-reporting there retries forever
// with no human told; and the workflow tables read agent prose rather than a
// transport error. Merging the answers changes retry and escalation behaviour
// at every one of those sites and needs per-site judgement. See the follow-up
// issue #3162.
//
// It is a leaf on purpose. internal/github depends on internal/clock alone, so
// anything heavier here would cycle.
package errclass

import "strings"

// The one genuinely shared table, reached through IsBadRef.
var (
	// The twelve needles internal/project and internal/workflow each carried byte-identically.
	badRefPhrases = []string{
		"bad object head",
		"fatal: bad object",
		"not a valid object name",
		"invalid object",
		"invalid revision range",
		"missing object",
		"unable to read sha1 file",
		"object file",
		"loose object",
		"unknown revision",
		"ambiguous argument",
		"reference broken",
	}
)

// Tables owned by a single caller. They live here so a reader sees all of
// them at once and can tell which phrase one table knows and another misses.
var (
	// github.IsTransientError, whose false answer escalates a poller.
	GitHubTransientPhrases = []string{
		"dial tcp",
		"i/o timeout",
		"context deadline exceeded",
		"connection reset",
		"connection refused",
		"tls handshake timeout",
		"no route to host",
	}

	// github.isTransientGHError, which reads raw gh output rather than a wrapped error.
	GHOutputTransientPhrases = []string{
		"http 502",
		"http 503",
		"http 504",
		"operation timed out",
		"i/o timeout",
		"deadline exceeded",
		"connection reset",
		"connection refused",
		"stream error",
		"unexpected eof",
		"tls handshake",
	}

	// github.isRateLimitedMessage.
	GitHubRateLimitPhrases = []string{
		"secondary rate limit",
		"api rate limit exceeded",
		"rate limit exceeded",
	}

	// github.isAuthErrorMsg. "gh auth login" stays narrow: the bare prefix also
	// matches gh's missing-scope hint, which would hold the shared circuit open.
	GitHubAuthPhrases = []string{
		"http 401",
		"bad credentials",
		"gh auth login",
		"gh_token environment variable",
	}

	// monitor.classifyGHError, which knows fewer phrases than github does.
	MonitorRateLimitPhrases = []string{
		"api rate limit exceeded",
		"secondary rate limit",
	}

	// project.IsTransientNetworkError, whose true answer must never escalate.
	GitTransportPhrases = []string{
		"connection refused",
		"connection reset",
		"connection timed out",
		"failed to connect",
		"couldn't connect to server",
		"could not resolve host",
		"couldn't resolve host",
		"network is unreachable",
		"operation timed out",
		"temporary failure in name resolution",
		"no route to host",
		"ssh: connect to host",
		"recv failure",
		"tls handshake timeout",
		"empty reply from server",
		"early eof",
		"unexpected disconnect while reading sideband packet",
	}

	// workflow.looksLikeTransientGitHub, which reads agent prose, so a bare
	// "timed out" and "tls:" are reachability signals rather than merge state.
	WorkflowTransientPhrases = []string{
		"connection refused",
		"connection reset",
		"could not resolve host",
		"no such host",
		"no route to host",
		"network is unreachable",
		"temporary failure in name resolution",
		"i/o timeout",
		"timed out",
		"context deadline exceeded",
		"tls handshake",
		"tls:",
	}

	// workflow.looksLikeAuthFailure, whose "gh auth" is broader than github's
	// because an echoed command is this site's usual credential signal.
	WorkflowAuthPhrases = []string{
		"bad credentials",
		"authentication failed",
		"failed to log in",
		"gh auth",
		"gh_token is invalid",
		"github_token is invalid",
		"token has expired",
		"could not read username for 'https://github.com'",
		"401 unauthorized",
	}

	// agent.classifyAgentError's rate_limit bucket.
	AgentRateLimitPhrases = []string{
		"rate limit",
		"429",
		"overloaded",
	}
)

// Matches reports whether text contains any phrase in families, comparing
// case-insensitively. Every table entry is lowercase for that reason.
func Matches(text string, families ...[]string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, family := range families {
		for _, phrase := range family {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
	}
	return false
}

// IsBadRef reports a corrupt or unresolvable git object or ref.
func IsBadRef(text string) bool {
	return Matches(text, badRefPhrases)
}
