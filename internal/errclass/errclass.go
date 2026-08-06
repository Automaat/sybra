// Package errclass is the one home for deciding whether an error is
// transient, rate limited, an auth failure, or permanent.
//
// The phrases used to live in twelve packages and drifted apart: "context
// deadline exceeded" was transient to internal/github and unknown to
// internal/project, a bare "tls handshake" was transient to two sites and not
// to two others, and internal/monitor matched GitHub's rate-limit text without
// lowercasing it first, so "Secondary rate limit" read as an ordinary failure
// and retried against an exhausted token.
//
// It is a leaf on purpose. internal/github depends on internal/clock alone, so
// anything heavier here would cycle.
package errclass

import "strings"

// Class is what a caller acts on. Unknown means the tables do not recognize
// the text, and callers must fail closed rather than assume permanence.
type Class string

const (
	Transient   Class = "transient"
	RateLimited Class = "rate_limited"
	Auth        Class = "auth"
	Permanent   Class = "permanent"
	Unknown     Class = "unknown"
)

// Phrase families. Each is the union of what the sites that were merged into
// this package used to match, so a caller composes the families its scope
// needs instead of carrying its own literals. Every entry is lowercase;
// matching lowercases the input once.
var (
	// NetworkPhrases are transport failures that resolve on their own.
	NetworkPhrases = []string{
		"dial tcp",
		"i/o timeout",
		"context deadline exceeded",
		"deadline exceeded",
		"connection reset",
		"connection refused",
		"timed out",
		"no route to host",
		"network is unreachable",
		"failed to connect",
		"couldn't connect to server",
		"empty reply from server",
		"recv failure",
	}

	// StreamPhrases are truncated-response failures. Kept out of
	// NetworkPhrases because they also appear in a genuine parse defect
	// ("parse graphql response: unexpected EOF"), which must escalate rather
	// than retry, so only callers reading raw transport output want them.
	StreamPhrases = []string{
		"stream error",
		"unexpected eof",
	}

	// DNSPhrases are name-resolution failures.
	DNSPhrases = []string{
		"could not resolve host",
		"couldn't resolve host",
		"no such host",
		"temporary failure in name resolution",
	}

	// TLSPhrases cover both the bare handshake failure and the timeout. Two
	// of the merged sites required " timeout" and two did not, so a
	// "tls: handshake failure" was transient to half the tree.
	TLSPhrases = []string{
		"tls handshake",
		"tls handshake timeout",
		"tls:",
	}

	// GatewayPhrases are the 5xx responses worth retrying. A plain 500 is
	// deliberately absent: it is usually a real server-side bug rather than
	// a blip, which is the narrower of the two answers the merged sites gave.
	GatewayPhrases = []string{
		"http 502",
		"http 503",
		"http 504",
	}

	// RateLimitPhrases are GitHub and provider backpressure.
	RateLimitPhrases = []string{
		"secondary rate limit",
		"api rate limit exceeded",
		"rate limit exceeded",
		"rate limit",
		"too many requests",
	}

	// AuthPhrases are credential failures. Unlike a rate limit these do not
	// self-heal, so callers bound their retries before escalating.
	AuthPhrases = []string{
		"http 401",
		"401 unauthorized",
		"bad credentials",
		"authentication failed",
		"failed to log in",
		"gh auth",
		"gh_token environment variable",
		"gh_token is invalid",
		"github_token is invalid",
		"token has expired",
		"could not read username for 'https://github.com'",
	}

	// BadRefPhrases are corrupt or unresolvable git object/ref errors, which
	// clear on a re-fetch. The same twelve lived byte-identically in
	// internal/project and internal/workflow.
	BadRefPhrases = []string{
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

	// GitTransportPhrases are git-remote transport failures with no
	// equivalent outside a fetch or push.
	GitTransportPhrases = []string{
		"ssh: connect to host",
		"early eof",
		"unexpected disconnect while reading sideband packet",
	}
)

// IsNetwork reports a transport or name-resolution failure.
func IsNetwork(text string) bool {
	return matches(text, NetworkPhrases, DNSPhrases, TLSPhrases)
}

// IsGitTransport reports a git-remote transport failure, including the
// generic network families.
func IsGitTransport(text string) bool {
	return matches(text, NetworkPhrases, DNSPhrases, TLSPhrases, GitTransportPhrases)
}

// IsBadRef reports a corrupt or unresolvable git object or ref.
func IsBadRef(text string) bool {
	return matches(text, BadRefPhrases)
}

// IsGateway reports a retryable 5xx gateway response.
func IsGateway(text string) bool {
	return matches(text, GatewayPhrases)
}

// IsRateLimit reports provider or forge backpressure.
func IsRateLimit(text string) bool {
	return matches(text, RateLimitPhrases)
}

// IsAuth reports a credential failure.
func IsAuth(text string) bool {
	return matches(text, AuthPhrases)
}

// Classify answers the four-way question for callers that want one verdict.
//
// Auth outranks rate limit, which outranks transient: a 401 that also mentions
// a retry budget is still a credential problem, and a rate limit needs a
// cooldown rather than an immediate retry. Text the tables do not recognize is
// Unknown, never Permanent — a caller that cannot tell must fail closed rather
// than swallow a retryable failure.
func Classify(text string) Class {
	switch {
	case IsAuth(text):
		return Auth
	case IsRateLimit(text):
		return RateLimited
	case IsGitTransport(text), IsGateway(text):
		return Transient
	default:
		return Unknown
	}
}

// ClassifyErr is Classify over an error. A nil error is Unknown, since there
// is nothing to classify.
func ClassifyErr(err error) Class {
	if err == nil {
		return Unknown
	}
	return Classify(err.Error())
}

// Matches reports whether text contains any phrase in families. Exported for
// callers whose scope is narrower than any single predicate here.
func Matches(text string, families ...[]string) bool {
	return matches(text, families...)
}

func matches(text string, families ...[]string) bool {
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

// Is reports whether err classifies as c. Kept so a caller reads as a question
// about one class rather than a switch.
func Is(err error, c Class) bool {
	return ClassifyErr(err) == c
}
