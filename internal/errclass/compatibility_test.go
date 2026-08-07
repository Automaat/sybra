package errclass

import (
	"fmt"
	"strings"
	"testing"
)

const compatibilityCorpusSize = 18_242

// TestDifferentialCompatibilityCorpus compares the pre-#3162 downstream
// decision at every migrated site with the policy result. The corpus size is
// fixed so adding a phrase cannot silently shrink the exercised surface.
// Only the two decisions named by #3162 are allowed to differ: GitHub's shared
// auth circuit learns "401 unauthorized", and monitor learns the complete
// GitHub rate-limit vocabulary.
func TestDifferentialCompatibilityCorpus(t *testing.T) {
	t.Parallel()
	corpus := recordedCompatibilityCorpus()
	if len(corpus) != compatibilityCorpusSize {
		t.Fatalf("corpus has %d inputs, want %d", len(corpus), compatibilityCorpusSize)
	}

	policies := []Policy{
		GitHubPollerRetryBiased,
		GHCommandEscalationBiased,
		MonitorCooldownBiased,
		GitHubTokenMintCooldownBiased,
		GitTransportEscalationBiased,
		WorkflowProseRetryBiased,
		AgentRecoveryBiased,
		AgentStreamRecoveryBiased,
		LLMExecRecoveryBiased,
		PRFixProseRetryBiased,
	}
	intended := map[string]int{"github-401-auth": 0, "monitor-rate-limit": 0}
	for _, input := range corpus {
		for _, policy := range policies {
			before := legacyDownstreamClass(input, policy)
			after := downstreamClass(Classify(input, policy), policy)
			if before == after {
				continue
			}
			kind, ok := intendedDifference(input, policy, before, after)
			if !ok {
				t.Errorf("unexpected differential for %q under %s: before=%s after=%s", input, policy, before, after)
				continue
			}
			intended[kind]++
		}
	}
	for kind, count := range intended {
		if count == 0 {
			t.Errorf("corpus did not exercise intended difference %s", kind)
		}
	}
	t.Logf("checked %d inputs across %d policies; intended differences: %v", len(corpus), len(policies), intended)
}

func downstreamClass(class Class, policy Policy) Class {
	switch policy {
	case GHCommandEscalationBiased, GitTransportEscalationBiased:
		if class == Transient {
			return Transient
		}
		return Unknown
	case MonitorCooldownBiased, GitHubTokenMintCooldownBiased:
		if class == RateLimited {
			return RateLimited
		}
		return Unknown
	case PRFixProseRetryBiased:
		if class == Transient {
			return Transient
		}
		return Unknown
	case AgentRecoveryBiased:
		if class == Transient || class == RateLimited {
			return class
		}
		return Unknown
	case AgentStreamRecoveryBiased, LLMExecRecoveryBiased:
		if class == RateLimited {
			return RateLimited
		}
		return Unknown
	default:
		return class
	}
}

func intendedDifference(input string, policy Policy, before, after Class) (string, bool) {
	lower := strings.ToLower(input)
	switch {
	case policy == GitHubPollerRetryBiased && before != Auth && after == Auth &&
		strings.Contains(lower, "401 unauthorized"):
		return "github-401-auth", true
	case policy == MonitorCooldownBiased && before == Unknown && after == RateLimited &&
		(strings.Contains(lower, "rate limit exceeded") || strings.Contains(lower, "github rate-limit wall")):
		return "monitor-rate-limit", true
	default:
		return "", false
	}
}

func legacyDownstreamClass(text string, policy Policy) Class {
	lower := strings.ToLower(text)
	contains := func(phrases ...string) bool {
		for _, phrase := range phrases {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
		return false
	}
	switch policy {
	case GitHubPollerRetryBiased:
		switch {
		case contains("github auth circuit open", "http 401", "bad credentials", "gh auth login", "gh_token environment variable"):
			return Auth
		case contains("github rate-limit wall", "secondary rate limit", "api rate limit exceeded", "rate limit exceeded"):
			return RateLimited
		case strings.Contains(lower, "http 5"), contains("dial tcp", "i/o timeout", "context deadline exceeded", "connection reset", "connection refused", "tls handshake timeout", "no route to host"):
			return Transient
		case contains("not mergeable", "required status check", "review is required", "changes requested", "waiting for status", "blocked by", "base branch policy prohibits the merge"):
			return Permanent
		default:
			return Unknown
		}
	case GHCommandEscalationBiased:
		if contains("http 502", "http 503", "http 504", "operation timed out", "i/o timeout", "deadline exceeded", "connection reset", "connection refused", "stream error", "unexpected eof", "tls handshake") {
			return Transient
		}
		return Unknown
	case MonitorCooldownBiased:
		if contains("api rate limit exceeded", "secondary rate limit") {
			return RateLimited
		}
		return Unknown
	case GitHubTokenMintCooldownBiased:
		if contains("rate limit") {
			return RateLimited
		}
		return Unknown
	case GitTransportEscalationBiased:
		if contains("connection refused", "connection reset", "connection timed out", "failed to connect", "couldn't connect to server", "could not resolve host", "couldn't resolve host", "network is unreachable", "operation timed out", "temporary failure in name resolution", "no route to host", "ssh: connect to host", "recv failure", "tls handshake timeout", "empty reply from server", "early eof", "unexpected disconnect while reading sideband packet") {
			return Transient
		}
		return Unknown
	case WorkflowProseRetryBiased:
		switch {
		case legacyWorkflowRateLimit(lower):
			return RateLimited
		case contains("connection refused", "connection reset", "could not resolve host", "no such host", "no route to host", "network is unreachable", "temporary failure in name resolution", "i/o timeout", "timed out", "context deadline exceeded", "tls handshake", "tls:"), hasWorkflowGatewayStatus(lower):
			return Transient
		case contains("bad credentials", "authentication failed", "failed to log in", "gh auth", "gh_token is invalid", "github_token is invalid", "token has expired", "could not read username for 'https://github.com'", "401 unauthorized"):
			return Auth
		default:
			return Unknown
		}
	case AgentRecoveryBiased:
		switch {
		case contains("clone", "fetch origin", "git fetch", "could not resolve host", "dial tcp", "i/o timeout", "dns"),
			strings.Contains(lower, "git") && strings.Contains(lower, "network"):
			return Transient
		case contains("rate limit", "429", "overloaded"):
			return RateLimited
		default:
			return Unknown
		}
	case AgentStreamRecoveryBiased:
		if contains("529", "overloaded") {
			return RateLimited
		}
		return Unknown
	case LLMExecRecoveryBiased:
		if hasStandaloneCode(lower, "529") || contains("overloaded") {
			return RateLimited
		}
		return Unknown
	case PRFixProseRetryBiased:
		switch {
		case contains("missing credential", "authentication", "permission denied"):
			return Unknown
		case contains("connection refused", "connection reset", "connection timed out", "failed to connect", "couldn't connect to server", "could not resolve host", "couldn't resolve host", "network is unreachable", "operation timed out", "temporary failure in name resolution", "no route to host", "ssh: connect to host", "recv failure", "tls handshake timeout", "empty reply from server", "early eof", "unexpected disconnect while reading sideband packet"),
			strings.Contains(lower, "remote unreachable"),
			strings.Contains(lower, "transport") && strings.Contains(lower, "github"):
			return Transient
		default:
			return Unknown
		}
	default:
		return Unknown
	}
}

func legacyWorkflowRateLimit(lower string) bool {
	if !strings.Contains(lower, "rate limit") {
		return false
	}
	return strings.Contains(lower, "github") || strings.Contains(lower, "graphql") ||
		strings.Contains(lower, "gh ") || strings.Contains(lower, "api rate limit") ||
		strings.Contains(lower, "secondary rate limit")
}

func recordedCompatibilityCorpus() []string {
	vocabulary := []string{
		"", "ordinary failure", "HTTP 500", "HTTP 502", "HTTP 503", "HTTP 504",
		"401 unauthorized", "HTTP 401", "bad credentials", "gh auth login",
		"github auth circuit open", "secondary rate limit", "api rate limit exceeded",
		"rate limit exceeded", "github rate-limit wall", "context deadline exceeded",
		"connection timed out", "operation timed out", "i/o timeout", "dial tcp",
		"connection reset", "connection refused", "could not resolve host", "no such host",
		"network is unreachable", "tls handshake timeout", "tls: certificate signed by unknown authority",
		"tls: first record does not look like a TLS handshake", "stream error", "unexpected EOF",
		"git fetch", "clone failed", "DNS", "429", "529", "1529", "overloaded", "waiting for status timed out",
		"required status check", "review is required", "base branch policy prohibits the merge",
		"permission denied", "authentication failed", "token has expired", "GitHub API rate limit",
		"missing credential", "remote unreachable", "GitHub transport failure",
	}
	templates := []string{
		"%s %s", "ERROR: %s (%s)", "prefix[%s]suffix %s", "before %s after %s", "%s; then %s",
		"(%s) unrelated=%s", "UPPER %s / lower %s", "%s\n%s",
	}
	out := make([]string, 0, compatibilityCorpusSize)
	for _, value := range vocabulary {
		out = append(out, value, strings.ToUpper(value), "wrapped: "+value)
	}
	for i := 0; len(out) < compatibilityCorpusSize; i++ {
		a := vocabulary[i%len(vocabulary)]
		b := vocabulary[(i*37+11)%len(vocabulary)]
		out = append(out, fmt.Sprintf(templates[i%len(templates)], a, b))
	}
	return out[:compatibilityCorpusSize]
}
