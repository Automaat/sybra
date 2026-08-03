package task

import (
	"errors"
	"fmt"
)

// allowedTransitions is the deny-by-default map of legal automated status
// moves, keyed by the task's current status. Self-transitions (from == to)
// are always legal and are handled by IsTransitionAllowed rather than
// enumerated here — hundreds of callers re-assert the current status while
// writing unrelated fields (StatusReason, Tags, ...).
//
// This is grounded in a full sweep of every production Apply/ApplyFn/
// ApplyStatusEffect call site (including the ~25 internal callers the
// workflow engine funnels through taskAdapter.UpdateTaskStatus/
// UpdateTaskBlocker) — see #2751. Escalation to human-required or blocked,
// and landing at done or cancelled via a merged/closed PR, are legitimate
// from virtually any active status, matching the system's actual recovery
// and PR-landing behavior. The two edges the task names explicitly
// (done -> in-progress, in-review -> todo) are deliberately absent.
//
// Reopening a terminal task (done/cancelled -> todo) is intentionally not a
// general rule: it exists only as an operator override (see
// TransitionIntent.OperatorOverride) at cli.reopen.
var allowedTransitions = map[Status]map[Status]bool{
	StatusNew: {
		StatusTodo:          true,
		StatusPlanning:      true,
		StatusHumanRequired: true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	StatusTodo: {
		StatusInProgress:    true,
		StatusPlanning:      true,
		StatusInReview:      true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		// monitor.routing.local_autoclose closes a scrubbed local
		// investigation task from whatever non-terminal status it is
		// currently sitting at, including a freshly created (still todo)
		// one — see internal/sybra/monitor_sink.go's findOpen, which only
		// excludes terminal statuses.
		StatusDone:      true,
		StatusCancelled: true,
	},
	StatusPlanning: {
		StatusPlanReview: true,
		// The watchdog's generic stall-retry path (internal/watchdog/agent.go)
		// writes StatusInProgress as a "resume this run" marker regardless of
		// the watched agent's role, including plan/plan-critic agents whose
		// task is still StatusPlanning — see applyStatusEffect's callers.
		StatusInProgress:    true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		StatusDone:          true,
	},
	StatusPlanReview: {
		StatusTodo:     true,
		StatusPlanning: true,
		// svc_planning.ApprovePlan's workflow-recovery path promotes a
		// completed plan-review workflow directly to in-progress in one
		// step, without an intermediate write to todo.
		StatusInProgress:    true,
		StatusHumanRequired: true,
		StatusDone:          true,
	},
	StatusInProgress: {
		StatusInReview:      true,
		StatusTodo:          true,
		StatusTesting:       true,
		StatusReadyReview:   true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	StatusReadyReview: {
		StatusInReview: true,
		StatusTesting:  true,
		// Recovery needs a route back to in-progress to run its fix agent.
		// Without it a branch that diverges while the task sits here cannot be
		// auto-resolved: branch-conflict-fix dispatches, the transition is
		// rejected, and the task escalates to a human with a message about
		// git rather than about the state machine that refused it.
		StatusInProgress:    true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	StatusInReview: {
		StatusTesting:       true,
		StatusReadyPR:       true,
		StatusInProgress:    true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	StatusTesting: {
		StatusReadyPR:       true,
		StatusInReview:      true,
		StatusInProgress:    true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	StatusReadyPR: {
		StatusInReview: true,
		// Same recovery route as ready-review: a divergence discovered at the
		// PR-opening stage must be fixable by an agent rather than parked.
		StatusInProgress:    true,
		StatusHumanRequired: true,
		StatusBlocked:       true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	StatusHumanRequired: {
		StatusTodo:        true,
		StatusPlanning:    true,
		StatusPlanReview:  true,
		StatusInProgress:  true,
		StatusReadyReview: true,
		StatusInReview:    true,
		StatusTesting:     true,
		StatusReadyPR:     true,
		StatusBlocked:     true,
		StatusDone:        true,
		StatusCancelled:   true,
	},
	StatusBlocked: {
		StatusTodo:          true,
		StatusInProgress:    true,
		StatusPlanning:      true,
		StatusInReview:      true,
		StatusHumanRequired: true,
		StatusDone:          true,
		StatusCancelled:     true,
	},
	// StatusDone and StatusCancelled are terminal: no automated exit. Reopening
	// is an operator-only action (see TransitionIntent.OperatorOverride).
	StatusDone:      {},
	StatusCancelled: {},
}

// IsTransitionAllowed reports whether an automated TransitionIntent may move
// a task from "from" to "to". Self-transitions are always legal; every other
// pair must be explicitly present in allowedTransitions.
func IsTransitionAllowed(from, to Status) bool {
	if from == to {
		return true
	}
	return allowedTransitions[from][to]
}

// ErrIllegalTransition is the sentinel a caller can match with errors.Is
// when Apply refuses to mutate a task because the requested from->to move is
// not in the allowed-transition table and the intent did not carry an
// OperatorOverride. Distinct from ErrTransitionConflict: a conflict means the
// caller's view of the task was stale, while an illegal transition means the
// move itself is never valid regardless of staleness.
var ErrIllegalTransition = errors.New("transition: move is not an allowed automated transition")

// IllegalTransitionError reports that a TransitionIntent requested a from->to
// move outside the allowed-transition table without an OperatorOverride.
// Unwrap yields ErrIllegalTransition for errors.Is checks.
type IllegalTransitionError struct {
	TaskID string
	From   Status
	To     Status
	Actor  string
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("transition: %q -> %q is not an allowed automated transition for task %s (actor %q)",
		e.From, e.To, e.TaskID, e.Actor)
}

func (e *IllegalTransitionError) Unwrap() error { return ErrIllegalTransition }
