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
}

// Budget bounds one task's automated review-role dispatches two ways:
// PerHour caps them in a rolling hour (catches a runaway loop regardless of
// which head SHA it targets); PerHead caps attempts against one specific PR
// head SHA (catches repeated review of an unchanged commit). Either field
// <=0 disables that half of the budget.
type Budget struct {
	PerHour int
	PerHead int
}

// HourlySpent counts review-role runs started within the last rolling hour.
func (b Budget) HourlySpent(runs []Run, now time.Time) int {
	cutoff := now.Add(-time.Hour)
	spent := 0
	for i := range runs {
		if runs[i].Role != ReviewRole {
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
