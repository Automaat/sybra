package provider

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
// claude provider as rate-limited or logged-out. The third return is the
// retry-after hint to use when setting a rate-limit cooldown; zero means the
// checker should fall back to its configured default.
//
// 529/overloaded is intentionally NOT classified here — the retry path in
// runner_headless.go already handles transient overload without marking the
// provider unhealthy.
func ClassifyClaudeError(s ErrorSample) (Signal, string, time.Duration) {
	if s.ErrorStatus == 401 || s.ErrorType == "authentication_error" || s.ErrorType == "invalid_api_key" {
		return SignalAuthFailure, "logged_out", 0
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	if containsAny(stderr, "not logged in", "please run claude auth login", "unauthorized") ||
		containsAny(content, "not logged in", "please run claude auth login") {
		return SignalAuthFailure, "logged_out", 0
	}
	// Weekly-limit phrasing is checked before the structured 429/rate_limit_error
	// short-circuit below: providers can attach a generic rate-limit error code
	// to a weekly-quota-exhaustion message, and the longer weeklyLimitCooldown
	// park only applies if the text is inspected first.
	if isWeeklyLimitText(stderr, content, s.ContentIsCleanResult) {
		return SignalRateLimit, "weekly_limit", weeklyLimitCooldown
	}
	if s.ErrorStatus == 429 || s.ErrorType == "rate_limit_error" || s.ErrorType == "credit_balance_too_low" {
		return SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0
	}
	if containsAny(stderr, "rate_limit", "rate limit", "credit_balance_too_low", "quota", "session limit", "usage limit", "weekly limit") ||
		containsRateLimitContent(content, s.ContentIsCleanResult,
			"rate_limit", "rate limit", "credit_balance_too_low", "quota", "session limit", "usage limit", "weekly limit") {
		return SignalRateLimit, "rate_limited", 0
	}
	return SignalNone, "", 0
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

// ClassifyCodexError mirrors ClassifyClaudeError for codex runs. Codex error
// taxonomy is less well-known at design time, so we lean on substring matching
// and let the runner log SignalNone cases with the raw strings for iterative
// discovery.
func ClassifyCodexError(s ErrorSample) (Signal, string, time.Duration) {
	if s.ErrorStatus == 401 || strings.EqualFold(s.ErrorType, "unauthorized") {
		return SignalAuthFailure, "logged_out", 0
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	if containsAny(stderr, "not logged in", "please run: codex login", "please run codex login", "unauthorized") ||
		containsAny(content, "not logged in", "please run: codex login") {
		return SignalAuthFailure, "logged_out", 0
	}
	// Weekly-limit phrasing is checked before the structured 429/rate_limit
	// short-circuit below: providers can attach a generic rate-limit error
	// code to a weekly-quota-exhaustion message, and the longer
	// weeklyLimitCooldown park only applies if the text is inspected first.
	if isWeeklyLimitText(stderr, content, s.ContentIsCleanResult) {
		return SignalRateLimit, "weekly_limit", weeklyLimitCooldown
	}
	// Host-anchored: a bare "websocket connection" without the codex backend
	// host must NOT match — it would false-positive on unrelated network
	// errors. Covers websocket refusals to the codex responses endpoint, MCP
	// transport failures, and model-list refresh timeouts, all of which are
	// Codex-backend infra blips rather than real task failures.
	//
	// The content-side match is restricted to non-clean results: a
	// successful (exit-0) run's assistant text can legitimately mention the
	// Codex backend host or quote a log line without that meaning the run
	// itself hit a connectivity failure — see containsRateLimitContent for
	// the same distinction on the rate-limit path.
	if containsAny(stderr, "chatgpt.com/backend-api", "failed to refresh available models") ||
		(!s.ContentIsCleanResult && containsAny(content, "chatgpt.com/backend-api", "failed to refresh available models")) {
		return SignalRateLimit, "connectivity", connectivityCooldown
	}
	if s.ErrorStatus == 429 || strings.EqualFold(s.ErrorType, "rate_limit") || strings.EqualFold(s.ErrorType, "insufficient_quota") {
		return SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0
	}
	if containsAny(stderr, "rate_limit", "rate limit", "insufficient_quota", "quota exceeded", "usage limit", "weekly limit") ||
		containsRateLimitContent(content, s.ContentIsCleanResult,
			"rate_limit", "rate limit", "insufficient_quota", "quota exceeded", "usage limit", "weekly limit") {
		return SignalRateLimit, "rate_limited", 0
	}
	return SignalNone, "", 0
}

// ClassifyCopilotError mirrors the other classifiers for GitHub Copilot CLI
// runs. Copilot has no documented machine error taxonomy, so this leans on
// substring matching of the phrases Copilot prints for auth and quota failures
// (kept in sync with isLoggedOutStderr). Without a copilot-specific classifier
// a logged-out/quota-exhausted copilot would return SignalNone and the health
// gate would keep routing failover work to a dead provider.
func ClassifyCopilotError(s ErrorSample) (Signal, string, time.Duration) {
	if s.ErrorStatus == 401 || strings.EqualFold(s.ErrorType, "unauthorized") {
		return SignalAuthFailure, "logged_out", 0
	}
	if s.ErrorStatus == 429 || strings.EqualFold(s.ErrorType, "rate_limit") || strings.EqualFold(s.ErrorType, "insufficient_quota") {
		return SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	authNeedles := []string{"not logged in", "not authenticated", "please run: copilot login", "run `copilot login`", "run 'copilot login'", "unauthorized"}
	if containsAny(stderr, authNeedles...) || containsAny(content, authNeedles...) {
		return SignalAuthFailure, "logged_out", 0
	}
	// Copilot meters usage in "premium requests"; an exhausted allowance is the
	// copilot analogue of a rate limit.
	quotaNeedles := []string{"rate_limit", "rate limit", "quota", "premium request", "usage limit", "monthly limit"}
	if containsAny(stderr, quotaNeedles...) || containsRateLimitContent(content, s.ContentIsCleanResult, quotaNeedles...) {
		return SignalRateLimit, "rate_limited", 0
	}
	return SignalNone, "", 0
}

func containsRateLimitContent(content string, cleanResult bool, broadNeedles ...string) bool {
	if !cleanResult {
		return containsAny(content, broadNeedles...)
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
