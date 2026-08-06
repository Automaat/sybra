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
	// watchdogSilentHangAvoidVarPrefix names the provider whose child went
	// silent on this step, so the very next dispatch of that step routes around
	// it. A silent hang used to park the provider on the health gate, and the
	// park is what pushed the retry onto a peer; dropping the park to keep the
	// provider available to other tasks would otherwise pin this run to the
	// provider that just produced nothing. Consumed once, by the next dispatch.
	watchdogSilentHangAvoidVarPrefix    = "watchdog.silent_hang_avoid."
	watchdogRewardHackingRetryVarPrefix = "watchdog.reward_hacking_retry."
	maxWatchdogRewardHackingRetries     = 1
	transientFetchRetryVarPrefix        = "transient_fetch.retry."
	maxTransientFetchRetries            = 2
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
