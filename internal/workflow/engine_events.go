package workflow

import "time"

const (
	watchdogHangRetryVarPrefix      = "watchdog.hang_retry."
	watchdogHangCleanRetryVarPrefix = "watchdog.hang_clean_retry."
	watchdogReaskNoteVar            = "watchdog_reask_note"
	maxWatchdogHangRetries          = 2
	watchdogStopRetryVarPrefix      = "watchdog.stop_retry."
	maxWatchdogStopRetries          = 2
	watchdogRateLimitRetryVarPrefix = "watchdog.rate_limit_retry."
	maxWatchdogRateLimitRetries     = 2
	// watchdogZeroOutputFreshRetryVarPrefix records that a zero-output stall was
	// already granted its one fresh-session round. A zero-output stall is a
	// poisoned resume, not a real rate limit; sybra#2542's StartedAt fence makes
	// a fresh dispatch succeed, but parking straight to blocked meant that fresh
	// retry never ran and a transient stall latched a permanent deadlock
	// (2026-07-23 board freeze). We fence, reset the budget, retry fresh once,
	// and only park blocked if the fresh round also exhausts.
	watchdogZeroOutputFreshRetryVarPrefix = "watchdog.rate_limit_fresh_retry."
	watchdogRewardHackingRetryVarPrefix   = "watchdog.reward_hacking_retry."
	maxWatchdogRewardHackingRetries       = 1
	transientFetchRetryVarPrefix          = "transient_fetch.retry."
	maxTransientFetchRetries              = 2
	// worktreeRepairRetryVarPrefix/maxWorktreeRepairRetries bound the automated
	// retry budget for tasks parked blocked with blocker.KindWorktreeRepair
	// (disk-space exhaustion or a failed rebase — see start_error.go). These
	// are machine-recoverable conditions (a disk-pressure reclaimer may have
	// freed space, or the branch may have moved) so ResumeStalled gets a
	// bounded number of automatic re-attempts before the task is marked
	// Exhausted and left for an operator, mirroring the watchdog-stop budget.
	worktreeRepairRetryVarPrefix   = "worktree_repair.retry."
	maxWorktreeRepairRetries       = 2
	circuitBreakerFailureVarPrefix = "circuit_breaker.failures."
	circuitBreakerFirstVarPrefix   = "circuit_breaker.first_failure."
	maxCircuitBreakerFailures      = 3
	circuitBreakerWindow           = 15 * time.Minute
)
