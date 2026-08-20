package provider

// Provider signal classification intentionally stays outside internal/errclass.
// It consumes structured provider-specific status/type fields, distinguishes a
// clean agent answer from provider stderr, and extracts quota reset hints that
// determine cooldown duration. It already gives every provider runner one
// centralized answer; reducing that evidence to an operational text policy
// would reintroduce the false quota parks guarded below.

import (
	"strings"
	"time"
)

// Signal classifies the relevance of a completed agent run's error to
// provider health state. Runners pass Signal back to the Manager so it can
// update the Checker without importing agent-specific types into this package.
type Signal int

const (
	SignalNone Signal = iota
	SignalRateLimit
	SignalAuthFailure
)

// connectivityCooldown is the retry-after hint for codex backend
// connectivity signals (websocket refusals, MCP transport failures, model
// list refresh timeouts) — short, since these tend to clear within a minute.
const connectivityCooldown = 60 * time.Second

// weeklyLimitCooldown is the retry-after hint for a weekly usage-limit
// signal — a long park, since the reset is hours away and a short cooldown
// would just churn retries against the same exhausted limit.
const weeklyLimitCooldown = time.Hour

// CooldownSource records where a park's duration came from, so an operator
// tracing a surprising window can tell a provider-stated instant from a
// configured guess — and from a provider message whose instant was rejected.
type CooldownSource string

const (
	CooldownFromConfig   CooldownSource = "configured_cooldown"
	CooldownFromProvider CooldownSource = "provider_hint"
	CooldownHintRejected CooldownSource = "provider_hint_rejected"
)

// rateLimitCooldown prefers a provider-supplied reset instant over the
// caller's default. Parsing runs on the raw (non-lowercased) sample so month
// names survive.
//
// A clean result supplies NO reset hint, from any surface. A successful exit-0
// run's content is the agent's own prose, which routinely discusses rate limits
// and dates — including the source of this very file. Its stderr is provider
// and tool warnings: an MCP server or a fallback-model notice can quote a usage
// limit while the run itself succeeded.
//
// Guarding content alone is not enough, and was not: the same defect
// reproduced through stderr, parking every enabled provider for 59 hours off
// two successful runs and leaving failover nowhere to go — against 15 minutes
// before this parsing existed. If the provider served the run, it did not
// refuse it, so nothing in that run states when it will serve again.
func rateLimitCooldown(s ErrorSample, fallback time.Duration) (time.Duration, CooldownSource) {
	if s.ContentIsCleanResult {
		return fallback, CooldownFromConfig
	}
	d, outcome := parseResetHint(s.Stderr)
	if outcome == HintNone {
		d, outcome = parseResetHint(s.Content)
	}
	switch outcome {
	case HintParsed:
		return d, CooldownFromProvider
	case HintRejected:
		return fallback, CooldownHintRejected
	default:
		return fallback, CooldownFromConfig
	}
}

// Classification is the result of inspecting a failed run: what it means for
// provider health, why, how long to park, and where that duration came from.
//
// One value rather than a positional tuple. The tuple was re-declared at every
// layer it crossed — the Provider interface, classifyProviderError,
// llmexec.classifyError, RecordProviderSignal, ReportProviderSignal,
// reportSignal, HealthGate.ReportRateLimit — plus every test fake. Adding the
// CooldownSource field alone touched ~10 production files and ~40 call sites,
// most of them carrying no information, and broke three test files in the
// mechanical edit. A named value makes the next field additive.
type Classification struct {
	Signal Signal
	// Reason is the short tag recorded on provider health and the agent.
	Reason string
	// RetryAfter is the park duration; zero means the caller's configured
	// cooldown.
	RetryAfter time.Duration
	// Source records whether RetryAfter came from the provider, a configured
	// default, or a provider hint that was rejected.
	Source CooldownSource
}

// classified is shorthand for building a Classification at a return site.
func classified(sig Signal, reason string, retryAfter time.Duration, src CooldownSource) Classification {
	return Classification{Signal: sig, Reason: reason, RetryAfter: retryAfter, Source: src}
}

// ErrorSample is the runner→classifier DTO. Using a plain struct (instead of
// agent.StreamEvent directly) prevents an import cycle between internal/agent
// and internal/provider.
type ErrorSample struct {
	Stderr               string
	ErrorType            string
	ErrorStatus          int
	Content              string
	ContentIsCleanResult bool
}

// ClassifyClaudeError decides whether a failed claude run should mark the
// claude provider as rate-limited or logged-out. Classification.RetryAfter is
// the hint to use when setting a rate-limit cooldown; zero means the checker
// should fall back to its configured default.
//
// 529/overloaded is intentionally NOT classified here — the retry path in
// runner_headless.go already handles transient overload without marking the
// provider unhealthy.
func ClassifyClaudeError(s ErrorSample) Classification {
	if s.ErrorStatus == 401 || s.ErrorType == "authentication_error" || s.ErrorType == "invalid_api_key" {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	if containsAny(stderr, "not logged in", "please run claude auth login", "unauthorized") ||
		containsAuthContent(content, s.ContentIsCleanResult, claudeAuthContentNeedles...) {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	// Weekly-limit phrasing is checked before the structured 429/rate_limit_error
	// short-circuit below: providers can attach a generic rate-limit error code
	// to a weekly-quota-exhaustion message, and the longer weeklyLimitCooldown
	// park only applies if the text is inspected first.
	if isWeeklyLimitText(stderr, content, s.ContentIsCleanResult) {
		cooldown, src := rateLimitCooldown(s, weeklyLimitCooldown)
		return classified(SignalRateLimit, "weekly_limit", cooldown, src)
	}
	if s.ErrorStatus == 429 || s.ErrorType == "rate_limit_error" || s.ErrorType == "credit_balance_too_low" {
		cooldown, src := rateLimitCooldown(s, 0)
		return classified(SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), cooldown, src)
	}
	if containsAny(stderr, "rate_limit", "rate limit", "credit_balance_too_low", "quota", "session limit", "usage limit", "weekly limit") ||
		containsRateLimitContent(content, s.ContentIsCleanResult,
			"rate_limit", "rate limit", "credit_balance_too_low", "quota", "session limit", "usage limit", "weekly limit") {
		cooldown, src := rateLimitCooldown(s, 0)
		return classified(SignalRateLimit, "rate_limited", cooldown, src)
	}
	return classified(SignalNone, "", 0, CooldownFromConfig)
}

// isWeeklyLimit reports whether text contains a weekly-specific limit
// phrase. A bare "resets ... at" fragment (present on session/usage limits
// too) is not sufficient — only an explicit "weekly" mention counts, so
// session/usage limits keep their short default cooldown instead of being
// parked for an hour.
func isWeeklyLimit(text string) bool {
	return containsAny(text,
		"weekly limit",
		"weekly limit reached",
		"hit your weekly limit",
		"reached your weekly limit",
	)
}

// isWeeklyLimitClean is the clean-result-safe variant of isWeeklyLimit: it
// drops the bare "weekly limit" needle, which a successful run's assistant
// text can legitimately contain (e.g. describing this very feature) without
// the run itself having hit a weekly quota.
func isWeeklyLimitClean(text string) bool {
	return containsAny(text,
		"weekly limit reached",
		"hit your weekly limit",
		"reached your weekly limit",
	)
}

// isWeeklyLimitText applies isWeeklyLimit to stderr (always, since stderr is
// never a clean success surface) and to content only under the appropriate
// guard for whether content came from a clean/successful result.
func isWeeklyLimitText(stderr, content string, contentIsCleanResult bool) bool {
	if isWeeklyLimit(stderr) {
		return true
	}
	if contentIsCleanResult {
		return isWeeklyLimitClean(content)
	}
	return isWeeklyLimit(content)
}

// isCodexConnectivityText reports whether text names a Codex-backend
// connectivity failure. "failed to refresh available models" matches on its
// own; the backend host string only counts alongside a connectivity-specific
// phrase (websocket/MCP transport), since a bare host mention can appear in
// unrelated errors that merely quote a URL.
func isCodexConnectivityText(text string) bool {
	if strings.Contains(text, "failed to refresh available models") {
		return true
	}
	return strings.Contains(text, "chatgpt.com/backend-api") &&
		containsAny(text, "websocket connection", "mcp transport")
}

// ClassifyCodexError mirrors ClassifyClaudeError for codex runs. Codex error
// taxonomy is less well-known at design time, so we lean on substring matching
// and let the runner log SignalNone cases with the raw strings for iterative
// discovery.
func ClassifyCodexError(s ErrorSample) Classification {
	if s.ErrorStatus == 401 || strings.EqualFold(s.ErrorType, "unauthorized") {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	if containsAny(stderr, "not logged in", "please run: codex login", "please run codex login", "unauthorized") ||
		containsAuthContent(content, s.ContentIsCleanResult, codexAuthContentNeedles...) {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	// Weekly-limit phrasing is checked before the structured 429/rate_limit
	// short-circuit below: providers can attach a generic rate-limit error
	// code to a weekly-quota-exhaustion message, and the longer
	// weeklyLimitCooldown park only applies if the text is inspected first.
	if isWeeklyLimitText(stderr, content, s.ContentIsCleanResult) {
		cooldown, src := rateLimitCooldown(s, weeklyLimitCooldown)
		return classified(SignalRateLimit, "weekly_limit", cooldown, src)
	}
	// Host-anchored: a bare "websocket connection" without the codex backend
	// host must NOT match — it would false-positive on unrelated network
	// errors. The host string alone is too broad the other way — text merely
	// mentioning the backend host without a connectivity-specific phrase
	// (e.g. quoting a URL in an unrelated error) must not match either — so
	// the host and a connectivity phrase are both required together.
	// "failed to refresh available models" is host-independent and matches
	// on its own. Together these cover websocket refusals to the codex
	// responses endpoint, MCP transport failures, and model-list refresh
	// timeouts, all of which are Codex-backend infra blips rather than real
	// task failures.
	//
	// The content-side match is restricted to non-clean results: a
	// successful (exit-0) run's assistant text can legitimately mention the
	// Codex backend host or quote a log line without that meaning the run
	// itself hit a connectivity failure — see containsRateLimitContent for
	// the same distinction on the rate-limit path.
	if isCodexConnectivityText(stderr) || (!s.ContentIsCleanResult && isCodexConnectivityText(content)) {
		return classified(SignalRateLimit, "connectivity", connectivityCooldown, CooldownFromConfig)
	}
	if s.ErrorStatus == 429 || strings.EqualFold(s.ErrorType, "rate_limit") || strings.EqualFold(s.ErrorType, "insufficient_quota") {
		cooldown, src := rateLimitCooldown(s, 0)
		return classified(SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), cooldown, src)
	}
	if containsAny(stderr, "rate_limit", "rate limit", "insufficient_quota", "quota exceeded", "usage limit", "weekly limit") ||
		containsRateLimitContent(content, s.ContentIsCleanResult,
			"rate_limit", "rate limit", "insufficient_quota", "quota exceeded", "usage limit", "weekly limit") {
		cooldown, src := rateLimitCooldown(s, 0)
		return classified(SignalRateLimit, "rate_limited", cooldown, src)
	}
	return classified(SignalNone, "", 0, CooldownFromConfig)
}

// ClassifyCopilotError mirrors the other classifiers for GitHub Copilot CLI
// runs. Copilot has no documented machine error taxonomy, so this leans on
// substring matching of the phrases Copilot prints for auth and quota failures
// (kept in sync with isLoggedOutStderr). Without a copilot-specific classifier
// a logged-out/quota-exhausted copilot would return SignalNone and the health
// gate would keep routing failover work to a dead provider.
func ClassifyCopilotError(s ErrorSample) Classification {
	if s.ErrorStatus == 401 || strings.EqualFold(s.ErrorType, "unauthorized") {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	if s.ErrorStatus == 429 || strings.EqualFold(s.ErrorType, "rate_limit") || strings.EqualFold(s.ErrorType, "insufficient_quota") {
		return classified(SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0, CooldownFromConfig)
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	authNeedles := []string{"not logged in", "not authenticated", "please run: copilot login", "run `copilot login`", "run 'copilot login'", "unauthorized"}
	if containsAny(stderr, authNeedles...) || containsAuthContent(content, s.ContentIsCleanResult, copilotAuthContentNeedles...) {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	// Copilot meters usage in "premium requests"; an exhausted allowance is the
	// copilot analogue of a rate limit.
	quotaNeedles := []string{"rate_limit", "rate limit", "quota", "premium request", "usage limit", "monthly limit"}
	if containsAny(stderr, quotaNeedles...) || containsRateLimitContent(content, s.ContentIsCleanResult, quotaNeedles...) {
		return classified(SignalRateLimit, "rate_limited", 0, CooldownFromConfig)
	}
	return classified(SignalNone, "", 0, CooldownFromConfig)
}

// ClassifyOpenCodeError classifies OpenCode CLI failures. OpenCode can front
// many providers, so keep the patterns generic and avoid provider-specific
// assumptions beyond common HTTP/auth/rate-limit vocabulary.
func ClassifyOpenCodeError(s ErrorSample) Classification {
	if s.ErrorStatus == 401 || s.ErrorStatus == 403 {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	if s.ErrorStatus == 429 {
		return classified(SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0, CooldownFromConfig)
	}
	authNeedles := []string{"not authenticated", "not logged in", "unauthorized", "invalid api key", "missing api key"}
	if containsAny(strings.ToLower(s.ErrorType), authNeedles...) ||
		containsAny(strings.ToLower(s.Stderr), authNeedles...) ||
		containsAuthContent(strings.ToLower(s.Content), s.ContentIsCleanResult, openCodeAuthContentNeedles...) {
		return classified(SignalAuthFailure, "logged_out", 0, CooldownFromConfig)
	}
	quotaNeedles := []string{"rate limit", "too many requests", "quota", "insufficient credits", "credit balance"}
	if containsAny(strings.ToLower(s.ErrorType), quotaNeedles...) ||
		containsAny(strings.ToLower(s.Stderr), quotaNeedles...) ||
		containsRateLimitContent(strings.ToLower(s.Content), s.ContentIsCleanResult, quotaNeedles...) {
		return classified(SignalRateLimit, "rate_limited", 0, CooldownFromConfig)
	}
	return classified(SignalNone, "", 0, CooldownFromConfig)
}

// containsAuthContent reports whether a run's terminal content is the provider
// refusing it on credentials. On many runs that content is the AGENT's own
// final message, and "not logged in" is exactly what an agent writes when it
// works on a login path — so the bare phrases that are right for the CLI's own
// error channel are wrong here. Only the anchored forms a provider actually
// prints count, and a run the provider served is never a refusal whatever its
// text says.
func containsAuthContent(content string, cleanResult bool, anchored ...string) bool {
	if cleanResult {
		return false
	}
	return containsAny(content, anchored...)
}

var claudeAuthContentNeedles = []string{
	"not logged in · please run", "not logged in. please run",
	"please run claude auth login", "please run /login",
}

var codexAuthContentNeedles = []string{
	"please run: codex login", "please run codex login",
	"not logged in. please run",
}

var copilotAuthContentNeedles = []string{
	"please run: copilot login", "run `copilot login`", "run 'copilot login'",
}

var openCodeAuthContentNeedles = []string{
	"invalid api key", "missing api key", "please run: opencode auth login",
	"run `opencode auth login`", "not logged in. please run",
}

// cleanResultLimitBudget bounds how long a clean run's content may be and
// still be read as a provider limit notice.
//
// A provider usage cap arrives on a subtype:"success" result with the limit
// text as essentially the whole content — see buildErrorSample, which captures
// the terminal result regardless of subtype precisely for that case. An
// agent's answer that happens to discuss rate limits is a full response with
// the phrase somewhere inside it. Length is what separates them: the needle
// list alone cannot, because "hit your usage limit" and "rate limit exceeded"
// are exactly what an agent writes when it works on this area of the codebase.
//
// Without this, a successful exit-0 run parked its own provider for the
// configured cooldown off its own prose.
const cleanResultLimitBudget = 400

func containsRateLimitContent(content string, cleanResult bool, broadNeedles ...string) bool {
	if !cleanResult {
		return containsAny(content, broadNeedles...)
	}
	if len(strings.TrimSpace(content)) > cleanResultLimitBudget {
		return false
	}
	return containsAny(content,
		"you've hit your session limit",
		"you have hit your session limit",
		"hit your session limit",
		"session limit reached",
		"reached your session limit",
		"you've hit your usage limit",
		"you have hit your usage limit",
		"hit your usage limit",
		"usage limit reached",
		"reached your usage limit",
		"you've hit your weekly limit",
		"you have hit your weekly limit",
		"hit your weekly limit",
		"weekly limit reached",
		"reached your weekly limit",
		"you've hit your rate limit",
		"you have hit your rate limit",
		"hit your rate limit",
		"rate limit reached",
		"reached your rate limit",
		"rate limit exceeded",
		"exceeded your rate limit",
		"insufficient_quota",
		"quota exceeded",
		"exceeded your current quota",
		"credit balance too low",
		"premium request allowance",
		"exceeded your premium request",
		"monthly limit reached",
	)
}

func reasonFromType(errType, fallback string) string {
	if strings.TrimSpace(errType) == "" {
		return fallback
	}
	return errType
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
