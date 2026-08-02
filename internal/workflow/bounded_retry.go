package workflow

import (
	"errors"
	"strconv"
)

// errRetryArmingSuperseded tells boundedRetry that a concurrent state change
// won after the counter was persisted but before the retry could dispatch.
// It is a normal fence, not a persistence failure.
var errRetryArmingSuperseded = errors.New("retry arming superseded by concurrent task update")

// boundedRetryPolicy is the shape shared by every capped auto-retry the
// engine runs against a stalled run_agent step: read a per-step attempt
// counter from workflow variables, and either arm one more attempt or
// escalate once the cap is spent. Before boundedRetry existed, each of the
// five watchdog/reschedule counters (hang, reward-hacking, rate-limit, stop,
// worktree-repair) plus the transient-fetch counter hand-copied this
// skeleton, so a guard fixed in one had to be copied by hand into the other
// five.
//
// Each policy still owns its own reason predicate, side-effect vars, and
// escalation target — those genuinely differ per counter (UpdateTaskStatus
// vs UpdateTaskBlocker, whether Workflow.State moves to ExecFailed, an extra
// fireComplete for the hang/ready-pr carve-out) — boundedRetry only unifies
// the counter read/increment/cap-compare shell and the persist+log tail.
type boundedRetryPolicy struct {
	// name identifies the policy in log lines: "workflow.<name>.persist" /
	// ".clear" / ".retry".
	name string
	// applies reports whether this policy owns the current stall (typed/
	// string reason predicate plus any structural guard such as step type).
	// false means boundedRetry is a no-op.
	applies func(e *Engine, t *TaskInfo, step *Step) bool
	// busy reports whether a tracked agent may still be mid-completion-
	// routing for this task+step, so this tick must not spend retry budget
	// or rewrite status out from under it. Optional.
	busy func(e *Engine, t *TaskInfo, step *Step) bool
	// counterKey returns the workflow variable holding this policy's
	// per-step attempt count.
	counterKey func(stepID string) string
	max        int
	// onArm sets any additional workflow variables a retry needs (reask
	// note, clean-retry baseline) once the counter has been bumped to
	// `attempt`, before the workflow is persisted. Optional.
	onArm func(e *Engine, t *TaskInfo, step *Step, attempt int)
	// onArmed fires after the incremented counter is durably persisted. It
	// owns clearing the task's status/blocker back to a runnable state —
	// each policy targets a different one — and syncing any TaskInfo field a
	// caller reads afterward (e.g. watchdog-stop's t.Status). Returning an
	// error aborts the tick (already logged as consumed). Optional.
	onArmed func(e *Engine, t *TaskInfo, step *Step, attempt int) error
	// onExhausted owns the entire escalation once the budget is spent:
	// workflow state, task status/blocker, logging, and any extra
	// completion signal. boundedRetry neither persists nor logs on its
	// behalf — policies disagree too much on shape to force a shared tail.
	onExhausted func(e *Engine, t *TaskInfo, step *Step, attempts int)
	// onPersistError overrides the default "workflow.<name>.persist" error
	// log when a policy's failure contract needs more than a log line (e.g.
	// checkpoint-reschedule also calls surfaceStartFailure to stay
	// consistent with its other dispatch-failure paths). Optional; nil uses
	// the standard log line.
	onPersistError func(e *Engine, t *TaskInfo, step *Step, err error)
}

// boundedRetry applies policy's counter/cap/escalate shape and reports
// whether this tick was consumed — either a retry was armed or the budget
// was exhausted and the task escalated — as opposed to left untouched for
// the caller to try some other handler or fall through to a fresh dispatch.
func (e *Engine) boundedRetry(t *TaskInfo, step *Step, p boundedRetryPolicy) bool {
	if t == nil || step == nil {
		return false
	}
	if !p.applies(e, t, step) {
		return false
	}
	if p.busy != nil && p.busy(e, t, step) {
		return false
	}

	retryKey := p.counterKey(step.ID)
	attempts := parseWorkflowInt(t.Workflow.Variables[retryKey])
	if attempts >= p.max {
		p.onExhausted(e, t, step, attempts)
		return true
	}

	attempt := attempts + 1
	t.Workflow.SetVar(retryKey, strconv.Itoa(attempt))
	if p.onArm != nil {
		p.onArm(e, t, step, attempt)
	}
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		if p.onPersistError != nil {
			p.onPersistError(e, t, step, err)
		} else {
			e.logger.Error("workflow."+p.name+".persist", "task_id", t.ID, "step", step.ID, "err", err)
		}
		return true
	}
	if p.onArmed != nil {
		if err := p.onArmed(e, t, step, attempt); err != nil {
			if errors.Is(err, errRetryArmingSuperseded) {
				e.logger.Debug("workflow."+p.name+".superseded", "task_id", t.ID, "step", step.ID)
				return true
			}
			e.logger.Error("workflow."+p.name+".clear", "task_id", t.ID, "step", step.ID, "err", err)
			return true
		}
	}
	e.logger.Info("workflow."+p.name+".retry",
		"task_id", t.ID, "step", step.ID, "attempt", attempt, "max", p.max)
	return false
}
