// Package reviewbudget is the single owner of the automated review loop's
// durable rate limits. Before this package existed, the same "is this task
// reviewing too much" question was answered three different ways: a per-hour
// breaker and a per-head-SHA dedup cap in internal/sybra (app_orchestrator.go),
// and a separate per-workflow-execution round cap in internal/workflow
// (simple-task-review.yaml's detect_tampering step). All three scanned the
// same signal — durable AgentRun history tagged Role=="review" — through
// different windows and different escape hatches. Budget is that signal's
// one owner: both internal/sybra (the inbound PR-review dispatcher) and
// internal/workflow (the pre-PR self-review loop) compute their gating
// decision through the same type, so a fix to one loop's runaway behavior
// (#2164) applies to both.
package reviewbudget

import "time"

// ReviewRole is the AgentRun role every review-loop dispatch is tagged with,
// whether it is internal/sybra's inbound PR-review workflow or
// simple-task-review's own code_review_simple/code_review_staff steps.
const ReviewRole = "review"

// Run is the minimal per-run signal Budget needs. Both internal/task.AgentRun
// and internal/workflow.AgentRunInfo carry a superset of these fields;
// callers adapt their own history slice into []Run rather than Budget
// depending on either package (avoiding an import cycle back into either).
type Run struct {
	Role      string
	StartedAt time.Time
	Outcome   string
	TurnCount int
	// Salvaged marks a run that left a review behind without finishing, which
	// no outcome value can express: the run failed and reviewed something.
	Salvaged bool
}

// SuccessOutcome is the AgentRun.Outcome a run records when it completed
// normally, mirroring task.RunOutcomeSuccess. Spelled here rather than
// imported so this package keeps depending on neither internal/task nor
// internal/workflow.
const SuccessOutcome = "success"

// dispatchedReview reports whether a run occupied a review dispatch slot,
// whatever came of it. A provider process that fails before its first
// assistant event never even saw its instructions, so it is not a dispatch the
// rolling-hour breaker should count against a runaway loop.
func dispatchedReview(run Run) bool {
	return run.Role == ReviewRole && (run.Outcome != "failure" || run.TurnCount != 0)
}

// reviewedTheChange reports whether a run actually produced a review. Only a
// successful run did: a revoked credential is reported on the run's own first
// turn, which is enough to look like work to a turn count but leaves no
// verdict behind, and a run stopped by a rate limit, a watchdog or a
// supersede records no outcome at all rather than a failure. A run whose
// partial review was salvaged is the exception: it failed and still reviewed
// something, so it is counted. The lifetime
// ceiling bounds how many times a task may really be reviewed, so it must be
// spent only on rounds that reviewed something.
func reviewedTheChange(run Run) bool {
	if run.Role != ReviewRole {
		return false
	}
	// A salvaged run is direct evidence it reviewed something, so it bypasses
	// the turn-count proxy the other cases lean on. The cost ceiling can stop a
	// review on its first assistant event, before anything increments that
	// count, and the proxy would then read the most expensive round there is as
	// a run that never started.
	return run.Salvaged || (dispatchedReview(run) && run.Outcome == SuccessOutcome)
}

// Budget bounds one task's automated review-role dispatches three ways:
// PerHour caps them in a rolling hour (catches a runaway loop regardless of
// which head SHA it targets); PerTask caps them across the task's lifetime
// (puts a hard ceiling on long-lived review/fix churn); PerHead caps attempts
// against one specific PR head SHA (catches repeated review of an unchanged
// commit). Any field <=0 disables that dimension.
type Budget struct {
	PerHour int
	PerTask int
	PerHead int
}

// HourlySpent counts review-role runs started within the last rolling hour.
func (b Budget) HourlySpent(runs []Run, now time.Time) int {
	cutoff := now.Add(-time.Hour)
	spent := 0
	for i := range runs {
		if !dispatchedReview(runs[i]) {
			continue
		}
		if runs[i].StartedAt.After(cutoff) {
			spent++
		}
	}
	return spent
}

// HourlyExceeded reports whether the rolling-hour review budget is spent.
func (b Budget) HourlyExceeded(runs []Run, now time.Time) bool {
	if b.PerHour <= 0 {
		return false
	}
	return b.HourlySpent(runs, now) >= b.PerHour
}

// LifetimeSpent counts the review-role runs in the task's durable history that
// actually reviewed the change.
func (b Budget) LifetimeSpent(runs []Run) int {
	spent := 0
	for i := range runs {
		if reviewedTheChange(runs[i]) {
			spent++
		}
	}
	return spent
}

// LifetimeExceeded reports whether the lifetime review budget is spent.
func (b Budget) LifetimeExceeded(runs []Run) bool {
	if b.PerTask <= 0 {
		return false
	}
	return b.LifetimeSpent(runs) >= b.PerTask
}

// Exhausted reports whether any review budget dimension is spent.
func (b Budget) Exhausted(runs []Run, now time.Time) bool {
	return b.HourlyExceeded(runs, now) || b.LifetimeExceeded(runs)
}

// HeadCovered reports whether head's review budget is already spent, given
// the task's last-reviewed head SHA and how many attempts it has already
// spent against it. An unknown head ("") counts as covered: we cannot tell a
// new push from the commit we just reviewed, and declining is the safe
// direction.
func (b Budget) HeadCovered(reviewedHeadSHA string, reviewedHeadAttempts int, head string) bool {
	if head == "" {
		return true
	}
	if reviewedHeadSHA != head {
		return false
	}
	if b.PerHead <= 0 {
		return false
	}
	return reviewedHeadAttempts >= b.PerHead
}

// NextAttempt returns the attempt number head is about to consume. A new
// head restarts the budget, so a real push always re-opens review.
func (b Budget) NextAttempt(reviewedHeadSHA string, reviewedHeadAttempts int, head string) int {
	if reviewedHeadSHA != head {
		return 1
	}
	return reviewedHeadAttempts + 1
}
