package watchdogreason

import "strings"

const (
	hangPrefix                 = "watchdog hang"
	rateLimitPrefix            = "watchdog: rate limit"
	rewardHackingPrefix        = "watchdog: reward_hacking"
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

// IsRetryableStop reports whether a human-required watchdog stop is a
// recoverable loop/stop verdict rather than a verified blocker. This is the
// class ResumeStalled may safely re-dispatch for workflow-owned implementation
// work, and umbrella rollup should treat as still in progress.
func IsRetryableStop(reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "watchdog stop" {
		return true
	}
	if !strings.HasPrefix(reason, "watchdog:") || IsRateLimit(reason) {
		return false
	}
	if reason == rewardHackingPrefix || strings.HasPrefix(reason, rewardHackingPrefix+":") {
		return false
	}
	if strings.HasPrefix(reason, verifyFailedPrefix) || strings.HasPrefix(reason, verifyUnconfirmedPrefix) {
		return false
	}
	return !strings.Contains(reason, retryBudgetExhaustedPhrase)
}
