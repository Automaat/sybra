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

// ErrorSample is the runner→classifier DTO. Using a plain struct (instead of
// agent.StreamEvent directly) prevents an import cycle between internal/agent
// and internal/provider.
type ErrorSample struct {
	Stderr      string
	ErrorType   string
	ErrorStatus int
	Content     string
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
	if s.ErrorStatus == 429 || s.ErrorType == "rate_limit_error" || s.ErrorType == "credit_balance_too_low" {
		return SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	if containsAny(stderr, "not logged in", "please run claude auth login", "unauthorized") ||
		containsAny(content, "not logged in", "please run claude auth login") {
		return SignalAuthFailure, "logged_out", 0
	}
	if containsAny(stderr, "rate_limit", "rate limit", "credit_balance_too_low", "quota") ||
		containsAny(content, "rate_limit", "rate limit", "credit_balance_too_low", "quota") {
		return SignalRateLimit, "rate_limited", 0
	}
	return SignalNone, "", 0
}

// ClassifyCodexError mirrors ClassifyClaudeError for codex runs. Codex error
// taxonomy is less well-known at design time, so we lean on substring matching
// and let the runner log SignalNone cases with the raw strings for iterative
// discovery.
func ClassifyCodexError(s ErrorSample) (Signal, string, time.Duration) {
	if s.ErrorStatus == 401 || strings.EqualFold(s.ErrorType, "unauthorized") {
		return SignalAuthFailure, "logged_out", 0
	}
	if s.ErrorStatus == 429 || strings.EqualFold(s.ErrorType, "rate_limit") || strings.EqualFold(s.ErrorType, "insufficient_quota") {
		return SignalRateLimit, reasonFromType(s.ErrorType, "rate_limited"), 0
	}
	stderr := strings.ToLower(s.Stderr)
	content := strings.ToLower(s.Content)
	if containsAny(stderr, "not logged in", "please run: codex login", "please run codex login", "unauthorized") ||
		containsAny(content, "not logged in", "please run: codex login") {
		return SignalAuthFailure, "logged_out", 0
	}
	if containsAny(stderr, "rate_limit", "rate limit", "insufficient_quota", "quota exceeded") ||
		containsAny(content, "rate_limit", "rate limit", "insufficient_quota", "quota exceeded") {
		return SignalRateLimit, "rate_limited", 0
	}
	return SignalNone, "", 0
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
