// Package errclass is the one home for the phrases that decide whether an
// error is transient, rate limited, or an auth failure.
//
// The tables used to live in a dozen packages and drifted apart. "context
// deadline exceeded" was transient to internal/github and unrecognized by
// internal/project. internal/monitor knew two of the four rate-limit phrases
// internal/github knew, so a gate-suppressed call retried as an ordinary
// failure. The twelve bad-ref needles existed byte-identically in
// internal/project and internal/workflow, free to diverge.
//
// Families are deliberately narrow, and a caller composes the ones its scope
// needs plus any literal only it uses. A broad family is not free: a bare
// "timed out" in the network family reclassified a permanently blocked merge
// as transient and cut its backoff ceiling from two hours to ten minutes, and
// a bare "tls:" made an x509 certificate failure look like a network blip that
// retries forever instead of reaching a human.
//
// It is a leaf on purpose. internal/github depends on internal/clock alone, so
// anything heavier here would cycle.
package errclass

import "strings"

// Class is the answer a caller acts on. Unknown means the tables do not
// recognize the text; callers fail closed rather than assume permanence.
type Class string

const (
	Transient   Class = "transient"
	RateLimited Class = "rate_limited"
	Auth        Class = "auth"
	Unknown     Class = "unknown"
)

// Phrase families. Every entry is lowercase; matching lowercases the input
// once. A phrase belongs here only when two or more callers used it; one used
// by a single caller stays with that caller.
var (
	// Every timeout entry is qualified: a bare "timed out" also matches a blocked merge waiting on a status check.
	NetworkPhrases = []string{
		"dial tcp",
		"i/o timeout",
		"context deadline exceeded",
		"connection reset",
		"connection refused",
		"connection timed out",
		"operation timed out",
		"no route to host",
		"network is unreachable",
		"failed to connect",
		"couldn't connect to server",
		"empty reply from server",
		"recv failure",
	}

	DNSPhrases = []string{
		"could not resolve host",
		"couldn't resolve host",
		"no such host",
		"temporary failure in name resolution",
	}

	// A bare "tls:" is excluded: it also matches x509 verification failures, which never self-heal.
	TLSPhrases = []string{
		"tls handshake",
	}

	// A plain 500 is absent because it is usually a real server-side bug rather than a blip.
	GatewayPhrases = []string{
		"http 502",
		"http 503",
		"http 504",
	}

	RateLimitPhrases = []string{
		"secondary rate limit",
		"rate limit exceeded",
	}

	// These do not self-heal, so callers bound their retries before escalating.
	AuthPhrases = []string{
		"http 401",
		"401 unauthorized",
		"bad credentials",
		"authentication failed",
		"failed to log in",
		"token has expired",
		"gh_token is invalid",
		"github_token is invalid",
		"could not read username for 'https://github.com'",
	}

	// Outside NetworkPhrases because a GraphQL parse defect reads the same and must escalate rather than retry.
	StreamPhrases = []string{
		"stream error",
		"unexpected eof",
	}

	GitTransportPhrases = []string{
		"ssh: connect to host",
		"early eof",
		"unexpected disconnect while reading sideband packet",
	}

	// Corrupt or unresolvable git objects and refs, which clear on a re-fetch.
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
)

// IsNetwork reports a transport, name-resolution, or TLS-handshake failure.
func IsNetwork(text string) bool {
	return matches(text, NetworkPhrases, DNSPhrases, TLSPhrases)
}

// IsGitTransport is IsNetwork plus the git-remote-only failures.
func IsGitTransport(text string) bool {
	return matches(text, NetworkPhrases, DNSPhrases, TLSPhrases, GitTransportPhrases)
}

// IsGateway reports a retryable 5xx gateway response.
func IsGateway(text string) bool {
	return matches(text, GatewayPhrases)
}

// IsRateLimit reports forge backpressure.
func IsRateLimit(text string) bool {
	return matches(text, RateLimitPhrases)
}

// IsAuth reports a credential failure.
func IsAuth(text string) bool {
	return matches(text, AuthPhrases)
}

// IsBadRef reports a corrupt or unresolvable git object or ref.
func IsBadRef(text string) bool {
	return matches(text, BadRefPhrases)
}

// Classify answers the three-way question, plus Unknown.
//
// Auth outranks rate limit, which outranks transient. A 401 that also mentions
// a retry budget is still a credential problem, and a rate limit needs a
// cooldown rather than an immediate retry.
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

// ClassifyErr is Classify over an error. A nil error is Unknown.
func ClassifyErr(err error) Class {
	if err == nil {
		return Unknown
	}
	return Classify(err.Error())
}

// Matches reports whether text contains any phrase in families. A caller whose
// scope is narrower than a predicate here composes families with it.
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
