package watchdogreason

import "strings"

const (
	hangPrefix                 = "watchdog hang"
	loopStopPrefix             = "watchdog: loop stop"
	budgetStopPrefix           = "watchdog: budget stop"
	rateLimitPrefix            = "watchdog: rate limit"
	rewardHackingPrefix        = "watchdog: reward_hacking"
	rewardHackingRetryPrefix   = "watchdog: reward-hacking retry"
	verifyFailedPrefix         = "watchdog: verify suite still fails after loop stop:"
	verifyUnconfirmedPrefix    = "watchdog: could not confirm agent stopped before verify"
	retryBudgetExhaustedPhrase = "retry budget exhausted"
)

func IsHang(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == hangPrefix || strings.HasPrefix(reason, hangPrefix+":")
}

func IsRateLimit(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == rateLimitPrefix || strings.HasPrefix(reason, rateLimitPrefix+":")
}

func LoopStop(reason string) string {
	return withDetail(loopStopPrefix, reason)
}

func BudgetStop(reason string) string {
	return withDetail(budgetStopPrefix, reason)
}

func RewardHacking(reason string) string {
	return withDetail(rewardHackingPrefix, reason)
}

// IsRetryableStop reports whether a human-required watchdog stop is a
// recoverable loop/stop verdict rather than a verified blocker. This is the
// class ResumeStalled may safely re-dispatch for workflow-owned implementation
// work, and umbrella rollup should treat as still in progress.
func IsRetryableStop(reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "watchdog stop" {
		return true
	}
	if strings.Contains(reason, retryBudgetExhaustedPhrase) {
		return false
	}
	if reason == loopStopPrefix || strings.HasPrefix(reason, loopStopPrefix+":") {
		return true
	}
	return isLegacyRetryableStop(reason)
}

func isLegacyRetryableStop(reason string) bool {
	if !strings.HasPrefix(reason, "watchdog: ") {
		return false
	}
	for _, prefix := range []string{
		budgetStopPrefix,
		rateLimitPrefix,
		rewardHackingPrefix,
		rewardHackingRetryPrefix,
		verifyFailedPrefix,
		verifyUnconfirmedPrefix,
	} {
		if strings.HasPrefix(reason, prefix) {
			return false
		}
	}
	return !strings.Contains(reason, "budget")
}

func withDetail(prefix, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return prefix
	}
	return prefix + ": " + detail
}
