package errclass

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

const (
	compatibilityCorpusSize   = 18_242
	compatibilityCorpusSHA256 = "ac80280282ec9c65731953d33ca7543e6d61f565192a6c96659b98bc2cce3042"
)

type downstreamDecision string

const (
	decisionDefault   downstreamDecision = "default"
	decisionRetry     downstreamDecision = "retry"
	decisionCooldown  downstreamDecision = "cooldown"
	decisionAuth      downstreamDecision = "auth-circuit"
	decisionMergeHeld downstreamDecision = "merge-held"
	decisionGit       downstreamDecision = "git-recovery"
	decisionCapacity  downstreamDecision = "capacity-recovery"
	decisionStop      downstreamDecision = "stop"
)

type compatibilitySite struct {
	name   string
	before func(string) downstreamDecision
	after  func(string) downstreamDecision
}

// TestDifferentialCompatibilityCorpus replays the pre-#3162 downstream
// decision order at every migrated operational surface, rather than comparing
// independent predicates or an invented global precedence. The input sequence
// reconstructs the corpus recorded for #3161: every pre-image literal in nine
// casing/token-boundary variants, representative real tool/transport bodies,
// Unicode case-fold probes, and fixed-seed mixed-phrase inputs. Its hash makes
// the recording immutable: changing the generator or ordering requires an
// explicit snapshot review.
//
// Only the two behavior changes named by #3162 are accepted: GitHub's auth
// circuit learns bare "401 unauthorized", and monitor learns the complete
// GitHub rate-limit vocabulary.
func TestDifferentialCompatibilityCorpus(t *testing.T) {
	t.Parallel()
	corpus := recordedCompatibilityCorpus()
	if len(corpus) != compatibilityCorpusSize {
		t.Fatalf("corpus has %d inputs, want %d", len(corpus), compatibilityCorpusSize)
	}
	hash := corpusHash(corpus)
	if hash != compatibilityCorpusSHA256 {
		t.Fatalf("corpus hash = %s, want recorded %s", hash, compatibilityCorpusSHA256)
	}

	intended := map[string]int{"github-401-auth": 0, "monitor-rate-limit": 0}
	for _, input := range corpus {
		for _, site := range compatibilitySites() {
			before, after := site.before(input), site.after(input)
			if before == after {
				continue
			}
			kind, ok := intendedDifference(input, site.name, before, after)
			if !ok {
				t.Errorf("unexpected differential for %q at %s: before=%s after=%s", input, site.name, before, after)
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
	t.Logf("checked %d recorded inputs across %d downstream sites; intended differences: %v", len(corpus), len(compatibilitySites()), intended)
}

func compatibilitySites() []compatibilitySite {
	return []compatibilitySite{
		{
			name: "github-review-fetch-retry-first",
			before: func(s string) downstreamDecision {
				switch {
				case legacyGitHubTransient(s):
					return decisionRetry
				case legacyGitHubAuth(s):
					return decisionAuth
				default:
					return decisionDefault
				}
			},
			after: func(s string) downstreamDecision {
				return githubDecision(Classify(s, GitHubPollerRetryBiased))
			},
		},
		{
			name: "github-poller-auth-first",
			before: func(s string) downstreamDecision {
				switch {
				case legacyGitHubAuth(s):
					return decisionAuth
				case legacyGitHubTransient(s):
					return decisionRetry
				default:
					return decisionDefault
				}
			},
			after: func(s string) downstreamDecision {
				return githubDecision(Classify(s, GitHubCircuitEscalationBiased))
			},
		},
		{
			name:   "github-auth-only",
			before: func(s string) downstreamDecision { return choose(legacyGitHubAuth(s), decisionAuth) },
			after: func(s string) downstreamDecision {
				return choose(Classify(s, GitHubCircuitEscalationBiased) == Auth, decisionAuth)
			},
		},
		{
			name:   "github-rate-cooldown",
			before: func(s string) downstreamDecision { return choose(legacyGitHubRate(s), decisionCooldown) },
			after: func(s string) downstreamDecision {
				return choose(Classify(s, MonitorCooldownBiased) == RateLimited, decisionCooldown)
			},
		},
		{
			name:   "gh-command-immediate-retry",
			before: func(s string) downstreamDecision { return choose(legacyGHCommandTransient(s), decisionRetry) },
			after: func(s string) downstreamDecision {
				return choose(Classify(s, GHCommandEscalationBiased) == Transient, decisionRetry)
			},
		},
		{
			name:   "monitor-rate-cooldown",
			before: func(s string) downstreamDecision { return choose(legacyMonitorRate(s), decisionCooldown) },
			after: func(s string) downstreamDecision {
				return choose(Classify(s, MonitorCooldownBiased) == RateLimited, decisionCooldown)
			},
		},
		{
			name:   "github-token-mint-cooldown",
			before: func(s string) downstreamDecision { return choose(containsLower(s, "rate limit"), decisionCooldown) },
			after: func(s string) downstreamDecision {
				return choose(Classify(s, GitHubTokenMintCooldownBiased) == RateLimited, decisionCooldown)
			},
		},
		{
			name:   "git-transport-escalation",
			before: func(s string) downstreamDecision { return choose(legacyGitTransport(s), decisionRetry) },
			after: func(s string) downstreamDecision {
				return choose(Classify(s, GitTransportEscalationBiased) == Transient, decisionRetry)
			},
		},
		{
			name:   "workflow-prose-routing",
			before: legacyWorkflowDecision,
			after:  func(s string) downstreamDecision { return workflowDecision(Classify(s, WorkflowProseRetryBiased)) },
		},
		{
			name:   "agent-fatal-recovery",
			before: legacyAgentDecision,
			after:  currentAgentDecision,
		},
		{
			name: "agent-stream-overload",
			before: func(s string) downstreamDecision {
				return choose(containsAnyLower(s, "529", "overloaded"), decisionCapacity)
			},
			after: func(s string) downstreamDecision {
				return choose(Classify(s, AgentStreamRecoveryBiased) == RateLimited, decisionCapacity)
			},
		},
		{
			name: "llmexec-overload",
			before: func(s string) downstreamDecision {
				return choose(hasStandaloneCode(strings.ToLower(s), "529") || containsLower(s, "overloaded"), decisionCapacity)
			},
			after: func(s string) downstreamDecision {
				return choose(Classify(s, LLMExecRecoveryBiased) == RateLimited, decisionCapacity)
			},
		},
		{
			name:   "pr-fix-recovery",
			before: legacyPRFixDecision,
			after: func(s string) downstreamDecision {
				return choose(Classify(s, PRFixProseRetryBiased) == Transient, decisionRetry)
			},
		},
		{
			name:   "merge-backoff",
			before: legacyMergeDecision,
			after:  currentMergeDecision,
		},
	}
}

func githubDecision(class Class) downstreamDecision {
	switch class {
	case Transient, RateLimited:
		return decisionRetry
	case Auth:
		return decisionAuth
	default:
		return decisionDefault
	}
}

func workflowDecision(class Class) downstreamDecision {
	switch class {
	case RateLimited:
		return decisionCooldown
	case Transient:
		return decisionRetry
	case Auth:
		return decisionAuth
	default:
		return decisionDefault
	}
}

func currentAgentDecision(s string) downstreamDecision {
	class := Classify(s, AgentRecoveryBiased)
	switch {
	case class == Transient:
		return decisionGit
	case containsAnyLower(s, "permission denied", "eacces", "operation not permitted"):
		return decisionStop
	case class == RateLimited:
		return decisionCapacity
	default:
		return decisionDefault
	}
}

func legacyWorkflowDecision(s string) downstreamDecision {
	lower := strings.ToLower(s)
	switch {
	case legacyWorkflowRateLimit(lower):
		return decisionCooldown
	case containsAny(lower, legacyWorkflowTransientPhrases()...) || hasWorkflowGatewayStatus(lower):
		return decisionRetry
	case containsAny(lower, legacyWorkflowAuthPhrases()...):
		return decisionAuth
	default:
		return decisionDefault
	}
}

func legacyAgentDecision(s string) downstreamDecision {
	lower := strings.ToLower(s)
	switch {
	case containsAny(lower, legacyAgentGitPhrases()...) || strings.Contains(lower, "git") && strings.Contains(lower, "network"):
		return decisionGit
	case containsAny(lower, "permission denied", "eacces", "operation not permitted"):
		return decisionStop
	case containsAny(lower, "rate limit", "429", "overloaded"):
		return decisionCapacity
	default:
		return decisionDefault
	}
}

func legacyPRFixDecision(s string) downstreamDecision {
	lower := strings.ToLower(s)
	if containsAny(lower, "missing credential", "authentication", "permission denied") {
		return decisionDefault
	}
	return choose(containsAny(lower, legacyGitTransportPhrases()...) ||
		strings.Contains(lower, "remote unreachable") ||
		strings.Contains(lower, "transport") && strings.Contains(lower, "github"), decisionRetry)
}

func legacyMergeDecision(s string) downstreamDecision {
	switch {
	case legacyGitHubAuth(s):
		return decisionAuth
	case legacyGitHubRate(s):
		return decisionCooldown
	case legacyGitHubNetworkTransient(s):
		return decisionRetry
	case containsAnyLower(s, legacyMergeBlockedPhrases()...):
		return decisionMergeHeld
	default:
		return decisionDefault
	}
}

func currentMergeDecision(s string) downstreamDecision {
	switch Classify(s, GitHubCircuitEscalationBiased) {
	case Auth:
		return decisionAuth
	case RateLimited:
		return decisionCooldown
	case Transient:
		return decisionRetry
	case Permanent:
		return decisionMergeHeld
	default:
		return decisionDefault
	}
}

func intendedDifference(input, site string, before, after downstreamDecision) (string, bool) {
	lower := strings.ToLower(input)
	switch {
	case strings.Contains(lower, "401 unauthorized") && before != decisionAuth && after == decisionAuth:
		return "github-401-auth", true
	case site == "monitor-rate-cooldown" && before == decisionDefault && after == decisionCooldown &&
		(strings.Contains(lower, "rate limit exceeded") || strings.Contains(lower, GitHubRateLimitWallMarker)):
		return "monitor-rate-limit", true
	default:
		return "", false
	}
}

func choose(ok bool, yes downstreamDecision) downstreamDecision {
	if ok {
		return yes
	}
	return decisionDefault
}

func legacyGitHubTransient(s string) bool {
	return legacyGitHubNetworkTransient(s) || legacyGitHubRate(s)
}

func legacyGitHubNetworkTransient(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "http 5") || containsAny(lower, legacyGitHubTransientPhrases()...)
}

func legacyGitHubAuth(s string) bool {
	return containsAnyLower(s, GitHubAuthCircuitMarker, "http 401", "bad credentials", "gh auth login", "gh_token environment variable")
}

func legacyGitHubRate(s string) bool {
	return containsAnyLower(s, GitHubRateLimitWallMarker, "secondary rate limit", "api rate limit exceeded", "rate limit exceeded")
}

func legacyMonitorRate(s string) bool {
	return containsAnyLower(s, "api rate limit exceeded", "secondary rate limit")
}

func legacyGHCommandTransient(s string) bool {
	return containsAnyLower(s, legacyGHCommandTransientPhrases()...)
}

func legacyGitTransport(s string) bool {
	return containsAnyLower(s, legacyGitTransportPhrases()...)
}

func legacyWorkflowRateLimit(lower string) bool {
	return strings.Contains(lower, "rate limit") &&
		(strings.Contains(lower, "github") || strings.Contains(lower, "graphql") ||
			strings.Contains(lower, "gh ") || strings.Contains(lower, "api rate limit") ||
			strings.Contains(lower, "secondary rate limit"))
}

func containsLower(s, phrase string) bool { return strings.Contains(strings.ToLower(s), phrase) }

func containsAnyLower(s string, phrases ...string) bool {
	return containsAny(strings.ToLower(s), phrases...)
}

func containsAny(lower string, phrases ...string) bool {
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func legacyGitHubTransientPhrases() []string {
	return []string{"dial tcp", "i/o timeout", "context deadline exceeded", "connection reset", "connection refused", "tls handshake timeout", "no route to host"}
}

func legacyGHCommandTransientPhrases() []string {
	return []string{"http 502", "http 503", "http 504", "operation timed out", "i/o timeout", "deadline exceeded", "connection reset", "connection refused", "stream error", "unexpected eof", "tls handshake"}
}

func legacyGitTransportPhrases() []string {
	return []string{"connection refused", "connection reset", "connection timed out", "failed to connect", "couldn't connect to server", "could not resolve host", "couldn't resolve host", "network is unreachable", "operation timed out", "temporary failure in name resolution", "no route to host", "ssh: connect to host", "recv failure", "tls handshake timeout", "empty reply from server", "early eof", "unexpected disconnect while reading sideband packet"}
}

func legacyWorkflowTransientPhrases() []string {
	return []string{"connection refused", "connection reset", "could not resolve host", "no such host", "no route to host", "network is unreachable", "temporary failure in name resolution", "i/o timeout", "timed out", "context deadline exceeded", "tls handshake", "tls:"}
}

func legacyWorkflowAuthPhrases() []string {
	return []string{"bad credentials", "authentication failed", "failed to log in", "gh auth", "gh_token is invalid", "github_token is invalid", "token has expired", "could not read username for 'https://github.com'", "401 unauthorized"}
}

func legacyAgentGitPhrases() []string {
	return []string{"clone", "fetch origin", "git fetch", "could not resolve host", "dial tcp", "i/o timeout", "dns"}
}

func legacyMergeBlockedPhrases() []string {
	return []string{"not mergeable", "required status check", "review is required", "changes requested", "waiting for status", "blocked by", "base branch policy prohibits the merge"}
}

func recordedCompatibilityCorpus() []string {
	phrases := compatibilityVocabulary()
	out := make([]string, 0, compatibilityCorpusSize)
	seen := make(map[string]struct{}, compatibilityCorpusSize)
	add := func(s string) {
		if len(out) == compatibilityCorpusSize {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	for _, phrase := range phrases {
		for _, variant := range phraseVariants(phrase) {
			add(variant)
		}
	}
	for _, sample := range recordedRealErrors() {
		add(sample)
	}

	rng := rand.New(rand.NewSource(0x3162))
	separators := []string{"; ", "\n", " | ", ": ", " ... ", " [cause] ", " / "}
	for len(out) < compatibilityCorpusSize {
		a := phrases[rng.Intn(len(phrases))]
		b := phrases[rng.Intn(len(phrases))]
		c := phrases[rng.Intn(len(phrases))]
		sep1 := separators[rng.Intn(len(separators))]
		sep2 := separators[rng.Intn(len(separators))]
		add(fmt.Sprintf("recorded-%05d: %s%s%s%s%s", len(out), a, sep1, b, sep2, c))
	}
	return out
}

func compatibilityVocabulary() []string {
	groups := [][]string{
		badRefPhrases,
		legacyGitHubTransientPhrases(),
		legacyGHCommandTransientPhrases(),
		{GitHubRateLimitWallMarker, "secondary rate limit", "api rate limit exceeded", "rate limit exceeded"},
		{GitHubAuthCircuitMarker, "http 401", "401 unauthorized", "bad credentials", "gh auth login", "gh_token environment variable"},
		legacyGitTransportPhrases(),
		legacyWorkflowTransientPhrases(),
		legacyWorkflowAuthPhrases(),
		legacyAgentGitPhrases(),
		{"rate limit", "429", "529", "1529", "overloaded"},
		legacyMergeBlockedPhrases(),
		{"x509:", "tls: first record does not look like a tls handshake", "missing credential", "authentication", "permission denied", "eacces", "operation not permitted", "remote unreachable", "github transport failure", "http 500", "http 502", "http 503", "http 504", "ordinary failure", ""},
	}
	var out []string
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, phrase := range group {
			if _, ok := seen[phrase]; ok {
				continue
			}
			seen[phrase] = struct{}{}
			out = append(out, phrase)
		}
	}
	return out
}

func phraseVariants(phrase string) []string {
	return []string{
		phrase,
		strings.ToUpper(phrase),
		"error: " + phrase,
		"prefix[" + phrase + "]suffix",
		"before " + phrase + " after",
		phrase + "\ncaused by: ordinary failure",
		"ordinary failure; " + phrase,
		"token=" + phrase,
		"İnput «" + phrase + "» Ω",
	}
}

func recordedRealErrors() []string {
	return []string{
		"gh: Bad credentials (HTTP 401)",
		"gh: Bad credentials; HTTP 503",
		"gh: Bad credentials; API rate limit exceeded",
		"GraphQL: API rate limit exceeded for user ID 1",
		"You have exceeded a secondary rate limit",
		"HTTP 403: rate limit exceeded",
		"Post https://api.github.com/graphql: dial tcp: i/o timeout",
		"Get https://api.github.com: context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
		"read tcp 127.0.0.1:443: connection reset by peer",
		"ssh: connect to host github.com port 22: Operation timed out",
		"fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com",
		"fatal: unable to access 'https://github.com/o/r/': TLS connect error",
		"tls: first record does not look like a TLS handshake",
		"x509: certificate signed by unknown authority",
		"error: RPC failed; curl 56 Recv failure: Connection reset by peer",
		"fetch-pack: unexpected disconnect while reading sideband packet",
		"fatal: early EOF",
		"waiting for status timed out",
		"merge blocked by required status check",
		"git fetch origin failed: API rate limit exceeded",
		"provider overloaded (529)",
		"processed 1529 tokens successfully",
		"could not read Username for 'https://github.com': terminal prompts disabled",
		"GH_TOKEN environment variable is invalid",
		"remote unreachable: GitHub transport failure",
	}
}

func corpusHash(corpus []string) string {
	h := sha256.New()
	for _, input := range corpus {
		_, _ = h.Write([]byte(input))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
