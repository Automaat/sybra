// Package errclass is the single operational error classifier.
//
// Callers choose a policy whose name states which classification mistake is
// safer at that site. The policies intentionally do not share one indiscriminate
// phrase union: pollers may retry broadly to avoid a board-wide escalation,
// while git transport must classify narrowly because a false transient answer
// can retry forever without reaching a human.
//
// Provider CLI signal parsing in internal/provider and telemetry aggregation in
// internal/selfmonitor are deliberately outside this classifier. The former
// consumes structured, provider-specific status fields and quota reset hints;
// the latter labels observations and does not make retry/cooldown/park decisions.
//
// This package remains a leaf because internal/github can otherwise create an
// import cycle.
package errclass

import "strings"

// Class is the four-way operational answer. Unknown is retained separately so
// an unrecognized phrase is never silently promoted to a permanent failure.
type Class string

const (
	Unknown     Class = "unknown"
	Transient   Class = "transient"
	RateLimited Class = "rate_limited"
	Auth        Class = "auth"
	Permanent   Class = "permanent"
)

// Synthetic GitHub errors use these canonical markers so their producers and
// the classifier cannot drift independently.
const (
	GitHubAuthCircuitMarker   = "github auth circuit open"
	GitHubRateLimitWallMarker = "github rate-limit wall"
)

// Bias documents which side of an ambiguous phrase is safer for a policy.
type Bias string

const (
	// RetryBiased accepts an occasional extra bounded retry to avoid escalating
	// a self-healing failure.
	RetryBiased Bias = "retry"
	// EscalationBiased accepts an occasional escalation to avoid an unbounded
	// retry or a retry against unchanged state.
	EscalationBiased Bias = "escalate"
	// CooldownBiased accepts an occasional cooldown to avoid spending more of a
	// shared rate-limit budget.
	CooldownBiased Bias = "cooldown"
	// RecoveryBiased accepts an occasional recovery dispatch to avoid burning a
	// task's terminal workflow retry budget.
	RecoveryBiased Bias = "recover"
)

// Policy identifies the input surface and its accepted error direction.
type Policy string

const (
	// GitHubPollerRetryBiased reads wrapped GitHub errors. A missed transient
	// answer escalates every affected poller, so this policy includes every 5xx
	// and gives transient evidence precedence over auth/rate-limit overlays.
	GitHubPollerRetryBiased Policy = "github-poller-retry-biased"
	// GitHubCircuitEscalationBiased reads the same wrapped GitHub errors at
	// callers that check authentication first. A mixed error must trip the
	// shared auth circuit instead of entering the ordinary transient retry path.
	GitHubCircuitEscalationBiased Policy = "github-circuit-escalation-biased"
	// GHCommandEscalationBiased reads raw gh output inside the immediate retry
	// loop. It excludes plain 500 and rate limits so retries do not amplify them.
	GHCommandEscalationBiased Policy = "gh-command-escalation-biased"
	// MonitorCooldownBiased reads direct gh issue-sink failures. A missed rate
	// limit spends the shared token, so it follows the complete GitHub set.
	MonitorCooldownBiased Policy = "monitor-cooldown-biased"
	// GitHubTokenMintCooldownBiased reads a 403 installation-token response.
	// The HTTP status supplies the GitHub context, so a bare rate-limit phrase
	// is sufficient and must cooldown rather than report bad credentials.
	GitHubTokenMintCooldownBiased Policy = "github-token-mint-cooldown-biased" //nolint:gosec // This names a policy; it is not a credential.
	// GitTransportEscalationBiased reads git fetch/ls-remote failures. Its
	// transient answer becomes a condition callers must never escalate.
	GitTransportEscalationBiased Policy = "git-transport-escalation-biased"
	// WorkflowProseRetryBiased reads agent prose, where bare timeout/TLS wording
	// is useful reachability evidence and every retry is bounded by workflow state.
	WorkflowProseRetryBiased Policy = "workflow-prose-retry-biased"
	// AgentRecoveryBiased reads fatal provider-run errors. Recovery labels avoid
	// consuming terminal workflow retry budget for capacity and clone failures.
	AgentRecoveryBiased Policy = "agent-recovery-biased"
	// AgentStreamRecoveryBiased preserves the provider stream's permissive
	// substring fallback when structured overload fields are absent.
	AgentStreamRecoveryBiased Policy = "agent-stream-recovery-biased"
	// LLMExecRecoveryBiased requires 529 to be a standalone status code so an
	// unrelated token count such as 1529 cannot fail over a provider.
	LLMExecRecoveryBiased Policy = "llmexec-recovery-biased"
	// PRFixProseRetryBiased reads a persisted worktree-preparation reason. A
	// false transient answer repeats recovery, but that retry is bounded; a
	// missed transient strands an otherwise recoverable PR-fix task.
	PRFixProseRetryBiased Policy = "pr-fix-prose-retry-biased"
)

// Bias reports the documented error direction for policy.
func (p Policy) Bias() Bias {
	switch p {
	case GitHubPollerRetryBiased, WorkflowProseRetryBiased, PRFixProseRetryBiased:
		return RetryBiased
	case GitHubCircuitEscalationBiased, GHCommandEscalationBiased, GitTransportEscalationBiased:
		return EscalationBiased
	case MonitorCooldownBiased, GitHubTokenMintCooldownBiased:
		return CooldownBiased
	case AgentRecoveryBiased, AgentStreamRecoveryBiased, LLMExecRecoveryBiased:
		return RecoveryBiased
	default:
		return ""
	}
}

var (
	badRefPhrases = []string{
		"bad object head", "fatal: bad object", "not a valid object name",
		"invalid object", "invalid revision range", "missing object",
		"unable to read sha1 file", "object file", "loose object",
		"unknown revision", "ambiguous argument", "reference broken",
	}

	githubTransientPhrases = []string{
		"dial tcp", "i/o timeout", "context deadline exceeded",
		"connection reset", "connection refused", "tls handshake timeout",
		"no route to host",
	}
	ghOutputTransientPhrases = []string{
		"http 502", "http 503", "http 504", "operation timed out",
		"i/o timeout", "deadline exceeded", "connection reset",
		"connection refused", "stream error", "unexpected eof", "tls handshake",
	}
	githubRateLimitPhrases = []string{
		GitHubRateLimitWallMarker, "secondary rate limit",
		"api rate limit exceeded", "rate limit exceeded",
	}
	githubAuthPhrases = []string{
		GitHubAuthCircuitMarker, "http 401", "401 unauthorized",
		"bad credentials", "gh auth login", "gh_token environment variable",
	}
	gitTransportPhrases = []string{
		"connection refused", "connection reset", "connection timed out",
		"failed to connect", "couldn't connect to server", "could not resolve host",
		"couldn't resolve host", "network is unreachable", "operation timed out",
		"temporary failure in name resolution", "no route to host",
		"ssh: connect to host", "recv failure", "tls handshake timeout",
		"empty reply from server", "early eof",
		"unexpected disconnect while reading sideband packet",
	}
	workflowTransientPhrases = []string{
		"connection refused", "connection reset", "could not resolve host",
		"no such host", "no route to host", "network is unreachable",
		"temporary failure in name resolution", "i/o timeout", "timed out",
		"context deadline exceeded", "tls handshake", "tls:",
	}
	workflowAuthPhrases = []string{
		"bad credentials", "authentication failed", "failed to log in", "gh auth",
		"gh_token is invalid", "github_token is invalid", "token has expired",
		"could not read username for 'https://github.com'", "401 unauthorized",
	}
	agentRateLimitPhrases = []string{"rate limit", "429", "overloaded"}
	agentGitPhrases       = []string{
		"clone", "fetch origin", "git fetch", "could not resolve host",
		"dial tcp", "i/o timeout", "dns",
	}
	mergeBlockedPhrases = []string{
		"not mergeable", "required status check", "review is required",
		"changes requested", "waiting for status", "blocked by",
		"base branch policy prohibits the merge",
	}
	explicitPermanentTransportPhrases = []string{
		"x509:", "tls: first record does not look like a tls handshake",
	}
	prFixPermanentPhrases = []string{
		"missing credential", "authentication", "permission denied",
	}
)

// Classify answers the operational error question once under policy. Auth
// outranks rate limiting, and rate limiting outranks an ordinary transient
// failure unless a policy documents legacy precedence (the agent clone bucket).
func Classify(text string, policy Policy) Class {
	if text == "" {
		return Unknown
	}
	lower := strings.ToLower(text)
	switch policy {
	case GitHubPollerRetryBiased:
		return classifyGitHubPollerRetryFirst(lower)
	case GitHubCircuitEscalationBiased:
		return classifyGitHubPollerAuthFirst(lower)
	case GHCommandEscalationBiased:
		return classifyGHCommand(lower)
	case MonitorCooldownBiased:
		return classifyMonitor(lower)
	case GitHubTokenMintCooldownBiased:
		return classifyTokenMint(lower)
	case GitTransportEscalationBiased:
		return classifyGitTransport(lower)
	case WorkflowProseRetryBiased:
		return classifyWorkflowProse(lower)
	case AgentRecoveryBiased:
		return classifyAgent(lower)
	case AgentStreamRecoveryBiased:
		return classifyAgentStream(lower)
	case LLMExecRecoveryBiased:
		return classifyLLMExec(lower)
	case PRFixProseRetryBiased:
		return classifyPRFixProse(lower)
	default:
		return Unknown
	}
}

func classifyGitHubPollerRetryFirst(lower string) Class {
	switch {
	case strings.Contains(lower, "http 5"), matchesLower(lower, githubTransientPhrases):
		return Transient
	case matchesLower(lower, githubRateLimitPhrases):
		return RateLimited
	case matchesLower(lower, githubAuthPhrases):
		return Auth
	case matchesLower(lower, mergeBlockedPhrases):
		return Permanent
	default:
		return Unknown
	}
}

func classifyGitHubPollerAuthFirst(lower string) Class {
	switch {
	case matchesLower(lower, githubAuthPhrases):
		return Auth
	case matchesLower(lower, githubRateLimitPhrases):
		return RateLimited
	case strings.Contains(lower, "http 5"), matchesLower(lower, githubTransientPhrases):
		return Transient
	case matchesLower(lower, mergeBlockedPhrases):
		return Permanent
	default:
		return Unknown
	}
}

func classifyGHCommand(lower string) Class {
	switch {
	case matchesLower(lower, ghOutputTransientPhrases):
		return Transient
	case matchesLower(lower, githubRateLimitPhrases):
		return RateLimited
	case matchesLower(lower, githubAuthPhrases):
		return Auth
	case strings.Contains(lower, "http 500"):
		return Permanent
	default:
		return Unknown
	}
}

func classifyMonitor(lower string) Class {
	switch {
	case matchesLower(lower, githubRateLimitPhrases):
		return RateLimited
	case matchesLower(lower, githubAuthPhrases):
		return Auth
	default:
		return Unknown
	}
}

func classifyTokenMint(lower string) Class {
	if strings.Contains(lower, "rate limit") {
		return RateLimited
	}
	return Unknown
}

func classifyGitTransport(lower string) Class {
	switch {
	case matchesLower(lower, gitTransportPhrases):
		return Transient
	case matchesLower(lower, githubAuthPhrases, workflowAuthPhrases):
		return Auth
	case matchesLower(lower, githubRateLimitPhrases):
		return RateLimited
	case matchesLower(lower, explicitPermanentTransportPhrases):
		return Permanent
	default:
		return Unknown
	}
}

func classifyWorkflowProse(lower string) Class {
	// Preserve workflow's decision order: cooldown, then bounded transient
	// retry, then bounded auth retry. Agent prose can mention more than one.
	switch {
	case isWorkflowRateLimit(lower):
		return RateLimited
	case matchesLower(lower, workflowTransientPhrases), hasWorkflowGatewayStatus(lower):
		return Transient
	case matchesLower(lower, workflowAuthPhrases):
		return Auth
	default:
		return Unknown
	}
}

func classifyAgent(lower string) Class {
	// Preserve the agent's legacy precedence: a git-fetch error that also
	// contains rate-limit prose belongs to the clone recovery bucket.
	switch {
	case matchesLower(lower, agentGitPhrases),
		strings.Contains(lower, "git") && strings.Contains(lower, "network"):
		return Transient
	case matchesLower(lower, agentRateLimitPhrases):
		return RateLimited
	default:
		return Unknown
	}
}

func classifyAgentStream(lower string) Class {
	if strings.Contains(lower, "529") || strings.Contains(lower, "overloaded") {
		return RateLimited
	}
	return Unknown
}

func classifyLLMExec(lower string) Class {
	if hasStandaloneCode(lower, "529") || strings.Contains(lower, "overloaded") {
		return RateLimited
	}
	return Unknown
}

func classifyPRFixProse(lower string) Class {
	switch {
	case matchesLower(lower, prFixPermanentPhrases):
		return Permanent
	case matchesLower(lower, gitTransportPhrases),
		strings.Contains(lower, "remote unreachable"),
		strings.Contains(lower, "transport") && strings.Contains(lower, "github"):
		return Transient
	default:
		return Unknown
	}
}

// ClassifyErr is Classify over err. Nil has no evidence and is Unknown.
func ClassifyErr(err error, policy Policy) Class {
	if err == nil {
		return Unknown
	}
	return Classify(err.Error(), policy)
}

// IsBadRef reports a corrupt or unresolvable git object or ref.
func IsBadRef(text string) bool {
	return matches(text, badRefPhrases)
}

func isWorkflowRateLimit(lower string) bool {
	if !strings.Contains(lower, "rate limit") {
		return false
	}
	return strings.Contains(lower, "github") ||
		strings.Contains(lower, "graphql") ||
		strings.Contains(lower, "gh ") ||
		strings.Contains(lower, "api rate limit") ||
		strings.Contains(lower, "secondary rate limit")
}

func hasWorkflowGatewayStatus(lower string) bool {
	for _, code := range []string{"502", "503"} {
		idx := strings.Index(lower, code)
		if idx < 0 {
			continue
		}
		if idx > 0 && isAlphaNumeric(lower[idx-1]) {
			continue
		}
		end := idx + len(code)
		if end < len(lower) && isAlphaNumeric(lower[end]) {
			continue
		}
		return true
	}
	return false
}

func isAlphaNumeric(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z'
}

func hasStandaloneCode(lower, code string) bool {
	for token := range strings.FieldsFuncSeq(lower, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'z')
	}) {
		if token == code {
			return true
		}
	}
	return false
}

func matches(text string, families ...[]string) bool {
	if text == "" {
		return false
	}
	return matchesLower(strings.ToLower(text), families...)
}

func matchesLower(lower string, families ...[]string) bool {
	for _, family := range families {
		for _, phrase := range family {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
	}
	return false
}
