package watchdogreason

import "strings"

type Kind string

const (
	hangPrefix                 = "watchdog hang"
	legacyStopReason           = "watchdog stop"
	loopStopPrefix             = "watchdog: loop stop"
	budgetStopPrefix           = "watchdog: budget stop"
	rateLimitPrefix            = "watchdog: rate limit"
	silentHangPrefix           = "watchdog: silent hang"
	rewardHackingPrefix        = "watchdog: reward_hacking"
	rewardHackingRetryPrefix   = "watchdog: reward-hacking retry"
	verifyFailedPrefix         = "watchdog: verify suite still fails after loop stop:"
	verifyUnconfirmedPrefix    = "watchdog: could not confirm agent stopped before verify"
	retryBudgetExhaustedPhrase = "retry budget exhausted"
	ZeroOutputBeforeStartup    = "zero output before startup timeout"
)

const (
	KindUnknown            Kind = ""
	KindHang               Kind = "hang"
	KindLoopStop           Kind = "loop_stop"
	KindBudgetStop         Kind = "budget_stop"
	KindRateLimit          Kind = "rate_limit"
	KindSilentHang         Kind = "silent_hang"
	KindRewardHacking      Kind = "reward_hacking"
	KindRewardHackingRetry Kind = "reward_hacking_retry"
	KindVerifyFailed       Kind = "verify_failed"
	KindVerifyUnconfirmed  Kind = "verify_unconfirmed"
)

type Parsed struct {
	Kind   Kind
	Detail string
}

func Parse(reason string) Parsed {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return Parsed{}
	case reason == hangPrefix:
		return Parsed{Kind: KindHang}
	case strings.HasPrefix(reason, hangPrefix+":"):
		return Parsed{Kind: KindHang, Detail: strings.TrimSpace(strings.TrimPrefix(reason, hangPrefix+":"))}
	case reason == legacyStopReason:
		return Parsed{Kind: KindLoopStop}
	case reason == loopStopPrefix:
		return Parsed{Kind: KindLoopStop}
	case strings.HasPrefix(reason, loopStopPrefix+":"):
		return Parsed{Kind: KindLoopStop, Detail: strings.TrimSpace(strings.TrimPrefix(reason, loopStopPrefix+":"))}
	case reason == budgetStopPrefix:
		return Parsed{Kind: KindBudgetStop}
	case strings.HasPrefix(reason, budgetStopPrefix+":"):
		return Parsed{Kind: KindBudgetStop, Detail: strings.TrimSpace(strings.TrimPrefix(reason, budgetStopPrefix+":"))}
	case reason == rateLimitPrefix:
		return Parsed{Kind: KindRateLimit}
	case strings.HasPrefix(reason, rateLimitPrefix+":"):
		return Parsed{Kind: KindRateLimit, Detail: strings.TrimSpace(strings.TrimPrefix(reason, rateLimitPrefix+":"))}
	case reason == silentHangPrefix:
		return Parsed{Kind: KindSilentHang}
	case strings.HasPrefix(reason, silentHangPrefix+":"):
		return Parsed{Kind: KindSilentHang, Detail: strings.TrimSpace(strings.TrimPrefix(reason, silentHangPrefix+":"))}
	case reason == rewardHackingPrefix:
		return Parsed{Kind: KindRewardHacking}
	case strings.HasPrefix(reason, rewardHackingPrefix+":"):
		return Parsed{Kind: KindRewardHacking, Detail: strings.TrimSpace(strings.TrimPrefix(reason, rewardHackingPrefix+":"))}
	case reason == rewardHackingRetryPrefix:
		return Parsed{Kind: KindRewardHackingRetry}
	case strings.HasPrefix(reason, rewardHackingRetryPrefix+":"):
		return Parsed{Kind: KindRewardHackingRetry, Detail: strings.TrimSpace(strings.TrimPrefix(reason, rewardHackingRetryPrefix+":"))}
	case strings.HasPrefix(reason, verifyFailedPrefix):
		return Parsed{Kind: KindVerifyFailed, Detail: strings.TrimSpace(strings.TrimPrefix(reason, verifyFailedPrefix))}
	case strings.HasPrefix(reason, verifyUnconfirmedPrefix):
		return Parsed{Kind: KindVerifyUnconfirmed, Detail: strings.TrimSpace(strings.TrimPrefix(reason, verifyUnconfirmedPrefix))}
	case isLegacyRetryableStop(reason):
		return Parsed{Kind: KindLoopStop, Detail: strings.TrimSpace(strings.TrimPrefix(reason, "watchdog:"))}
	default:
		return Parsed{Detail: reason}
	}
}

func IsHang(reason string) bool {
	return Parse(reason).Kind == KindHang
}

func IsRateLimit(reason string) bool {
	return Parse(reason).Kind == KindRateLimit
}

func IsRewardHackingRetry(reason string) bool {
	return Parse(reason).Kind == KindRewardHackingRetry
}

// IsSilentHang reports whether reason marks a run that produced no output at
// all before the startup timeout.
//
// Two forms match. The current one is KindSilentHang. The second is the legacy
// rate-limit wrapping this case used before it got its own kind: tasks parked
// under the old form are already persisted on disk, and their recovery keys off
// this predicate, so dropping the legacy match would strand every one of them
// on the next upgrade.
func IsSilentHang(reason string) bool {
	parsed := Parse(reason)
	if parsed.Kind == KindSilentHang {
		return true
	}
	return parsed.Kind == KindRateLimit && parsed.Detail == ZeroOutputBeforeStartup
}

func Hang(reason string) string {
	return withDetail(hangPrefix, reason)
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

func RewardHackingRetry(reason string) string {
	return withDetail(rewardHackingRetryPrefix, reason)
}

// IsRewardHacking reports whether reason is the terminal, non-retryable
// reward-hacking verdict. It deliberately excludes rewardHackingRetryPrefix:
// that status is owned by ResumeStalled's bounded clean re-dispatch path.
func IsRewardHacking(reason string) bool {
	return Parse(reason).Kind == KindRewardHacking
}

func RateLimit(reason string) string {
	return withDetail(rateLimitPrefix, reason)
}

func SilentHang(reason string) string {
	return withDetail(silentHangPrefix, reason)
}

// IsRetryableStop reports whether a human-required watchdog stop is a
// recoverable loop/stop verdict rather than a verified blocker. This is the
// class ResumeStalled may safely re-dispatch for workflow-owned implementation
// work, and umbrella rollup should treat as still in progress.
func IsRetryableStop(reason string) bool {
	parsed := Parse(reason)
	if strings.Contains(strings.TrimSpace(reason), retryBudgetExhaustedPhrase) {
		return false
	}
	return parsed.Kind == KindLoopStop
}

func isLegacyRetryableStop(reason string) bool {
	if !strings.HasPrefix(reason, "watchdog: ") {
		return false
	}
	for _, prefix := range []string{
		budgetStopPrefix,
		rateLimitPrefix,
		silentHangPrefix,
		rewardHackingPrefix,
		rewardHackingRetryPrefix,
		verifyFailedPrefix,
		verifyUnconfirmedPrefix,
	} {
		if strings.HasPrefix(reason, prefix) {
			return false
		}
	}
	return strings.Contains(strings.ToLower(reason), "loop")
}

func withDetail(prefix, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return prefix
	}
	return prefix + ": " + detail
}
