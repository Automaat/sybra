package workflow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

func (e *Engine) shouldSkipResumeForRateLimitedProvider(t *TaskInfo, step *Step, logEvent string) bool {
	// Don't re-dispatch a single-agent step to the same provider while it is
	// rate-limited; do continue when failover can route this run to a healthy
	// peer. Parallel children are checked at child spawn time because each child
	// can use a different provider.
	if step.Type != StepRunAgent {
		return false
	}
	prov := resolveProvider(step.Config.Provider, t.Workflow, e.agents.DefaultProvider(), *t)
	if prov == "" {
		// resolveProvider yields "" for a step with no provider key and for
		// `ab`/`cross` without provenance — 9 of the 14 builtin run_agent steps.
		// ProviderRateLimited substitutes the default internally, so the line is
		// already about that provider and would otherwise refuse to name it.
		prov = e.agents.DefaultProvider()
	}
	if !e.agents.ProviderRateLimited(prov) || e.agents.ProviderCanFailover(prov) {
		return false
	}
	// Deduped, not Debug: a park can now last days (provider-stated reset
	// instants land three days out), so the direct Debug call was both
	// invisible at the default level and 3,600 lines per task at debug level.
	// Keying the value on the provider re-arms INFO when the park moves.
	e.resumeSkip.Log(e.logger, logEvent, t.ID, "provider_rate_limited|"+prov+"|"+step.ID,
		"task_id", t.ID, "reason", "provider_rate_limited", "provider", prov, "step", step.ID)
	return true
}

func workflowRetryAfter(wf *Execution) (time.Time, bool) {
	if wf == nil || wf.Variables == nil {
		return time.Time{}, false
	}
	raw := wf.Variables[workflowRetryAfterVar]
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

// isShutdownCancellationGate short-circuits surfaceStartFailure ahead of the
// rebase-conflict-recovery branch further down: a context cancellation
// tracing back to the engine's own shutdown context (e.ctx) is not a
// task-attributable failure — one graceful restart cancels every in-flight
// git/agent operation across every concurrently-dispatching task at once.
// Placement matters: a shutdown-cancelled fetch inside reconcileAndRebase's
// ReconcileWithRemote step wraps ErrRebaseFailed rather than
// ErrTransientFetch (project.IsTransientNetworkError doesn't recognize
// "context canceled" as a transient network blip), so a check placed only in
// surfaceStartFailureClassified would still let this case fall into the
// conflict-recovery branch below and dispatch real branch-conflict-fix
// agents — doomed to be cancelled by that same shutdown — on top of
// mass-tripping the circuit breaker this fix primarily targets. Suppressed
// exactly like the other benign dispatch-plumbing sentinels
// surfaceStartFailureClassified handles below (ErrDispatchInFlight et al.):
// no status write, no breaker increment, no recovery dispatch (sybra#2291).
func (e *Engine) isShutdownCancellationGate(taskID, stepID string, err error) bool {
	if !e.isShutdownCancellation(err) {
		return false
	}
	e.logger.Info("workflow.start-failure.shutdown-cancellation.suppress",
		"task_id", taskID, "step", stepID)
	return true
}

// surfaceStartFailure writes a human-readable reason to task.StatusReason
// when ResumeStalled fails to (re-)dispatch a step's agent. Permanent errors
// (e.g. project missing) also flip the task to human-required so the resume
// loop stops retrying every minute and the UI surfaces the block.
//
// Idempotent: writing the same reason twice is a no-op for the user — the
// task already shows it. The transient branch deliberately does not change
// status, so retries continue.
//
// wf and stepID feed the circuit breaker below; pass the caller's Execution
// and the failing step's ID so repeated failures for that (task, step) are
// tracked. Either may be zero-valued (nil wf, empty stepID) for callers that
// don't have them handy — the breaker simply stays inactive for that call.
func (e *Engine) surfaceStartFailure(taskID string, currentStatus taskstatus.Status, err error, wf *Execution, stepID string) {
	if e.isShutdownCancellationGate(taskID, stepID, err) {
		return
	}
	// A pre-agent-start rebase failure is the same "task branch conflicts
	// with base" condition push_branch/create_pr hit further down the
	// pipeline (see pushTaskBranch's project.ErrDivergedNeedsResolve branch) —
	// try the same autonomous branch-conflict-fix recovery before parking the
	// task on a human. currentStatus != "human-required" mirrors the sticky
	// guard below: don't re-trigger recovery for a task a concurrent handler
	// already parked. A disk-space-caused rebase failure skips recovery
	// entirely — a full host disk is not a content conflict a conflict-fix
	// agent can resolve, so routing it through recovery would just waste an
	// agent run before falling back to the same human-required park anyway.
	// See #1856.
	if currentStatus != taskstatus.HumanRequired && e.conflictRecovery != nil &&
		errors.Is(err, worktreeerr.ErrRebaseFailed) && !worktreeerr.IsDiskSpaceError(err) {
		e.logger.Info("workflow.start-failure.branch-conflict.recover", "task_id", taskID, "step", stepID)
		if e.tryConflictRecoveryWithFallback(taskID, func() {
			e.surfaceStartFailureClassified(taskID, currentStatus, err, wf, stepID)
		}) {
			return
		}
		// Recovery declined (e.g. no linked PR, retry budget exhausted) — fall
		// through to the normal classification/escalation below.
	}
	e.surfaceStartFailureClassified(taskID, currentStatus, err, wf, stepID)
}

func (e *Engine) surfaceStartFailureClassified(taskID string, currentStatus taskstatus.Status, err error, wf *Execution, stepID string) {
	failure := ClassifyAgentStartFailure(err)
	if failure.Reason == "" {
		return
	}
	// Sticky: a task already parked at human-required must not be touched
	// again from here. Without this, a call driven by a stale pre-dispatch
	// status snapshot could rewrite a status a concurrent handler resolved
	// to something more specific (e.g. in_review) back to human-required.
	if currentStatus == taskstatus.HumanRequired {
		return
	}
	target := currentStatus
	if failure.Permanent {
		target = taskstatus.HumanRequired
		if !failure.Blocker.IsZero() && !blocker.AllowsHumanRequired(failure.Blocker.Kind) {
			target = taskstatus.Blocked
		}
	}
	// A provider being rate-limited right now is a transient capacity condition,
	// not a genuine start failure: counting it toward the breaker would trip a
	// task to human-required for something that self-heals when the cooldown
	// window expires. Only genuine failures (bad worktree, missing project,
	// crashes) — and auth failures that need a human login — feed the breaker.
	// isDeferredNotFailed excludes resource-pressure defers the same way: they
	// surface a status_reason (unlike the fully-silent transient sentinels)
	// but must never accumulate toward the breaker.
	if wf != nil && stepID != "" && !isTransientCapacityError(err) && !isDeferredNotFailed(err) {
		attempts, trip := recordCircuitBreakerFailure(wf, stepID, time.Now())
		if trip {
			// The status skip in resumeSkipReasonForStatus alone is not a
			// sufficient backstop against a flapping loop: it only stops
			// ResumeStalled from touching a task that is CURRENTLY
			// human-required, but does nothing once some other path (a
			// racing recovery branch, a status-change hook, a future bug)
			// flips it back off human-required. Marking the workflow
			// ExecFailed makes the halt independent of task.Status entirely
			// — every resume path in this file (ResumeStalled,
			// RescheduleRateLimitedAgent, HandleStatusChange) already
			// refuses to touch a workflow whose State is ExecFailed.
			wf.State = ExecFailed
			target = taskstatus.HumanRequired
			if !failure.Blocker.IsZero() && !blocker.AllowsHumanRequired(failure.Blocker.Kind) {
				target = taskstatus.Blocked
			}
			failure.Reason = fmt.Sprintf("circuit breaker: %s (tripped after %d dispatch failures for step %q within %s)",
				failure.Reason, attempts, stepID, circuitBreakerWindow)
			e.logger.Warn("workflow.circuit-breaker.tripped",
				"task_id", taskID, "step", stepID, "attempts", attempts)
		}
		if setErr := e.tasks.SetWorkflow(taskID, wf); setErr != nil {
			e.logger.Error("workflow.circuit-breaker.persist", "task_id", taskID, "step", stepID, "err", setErr)
		}
	}
	var uErr error
	if failure.Blocker.IsZero() {
		uErr = e.tasks.UpdateTaskStatus(taskID, target, failure.Reason)
	} else {
		uErr = e.tasks.UpdateTaskBlocker(taskID, target, failure.Reason, failure.Blocker)
	}
	if uErr != nil {
		e.logger.Error("workflow.resume-stalled.surface", "task_id", taskID, "err", uErr)
	}
}

// isShutdownCancellation applies IsShutdownCancellation's correlation
// heuristic against e.ctx: err wraps context.Canceled while the engine's own
// shutdown context is currently done. e.ctx is bound exactly once, from the
// app's root context (App.Startup -> Engine.SetContext), and is the same
// context object agentorch.Orchestrator uses to cancel worktree/git
// operations on shutdown.
func (e *Engine) isShutdownCancellation(err error) bool {
	return IsShutdownCancellation(e.ctx, err)
}

// IsShutdownCancellation is a correlation heuristic, not causality proof: it
// reports whether ctx is currently done AND err wraps context.Canceled. It
// cannot verify err's cancellation actually originated from ctx specifically
// (vs. some other cancelled context in the same call chain) — but a
// different, unrelated context's own timeout would surface as
// context.DeadlineExceeded or a non-context error instead, so in practice
// this reliably distinguishes "we are shutting down" from a genuine failure.
// Exported so internal/recovery's independent stale-task restart path —
// which surfaces dispatch failures through its own
// Recovery.surfaceStartFailure rather than going through Engine — can apply
// the identical shutdown-vs-genuine-failure heuristic to the
// "restart-stale.failed" log line named in sybra#2291, rather than
// maintaining a second, drifting copy of this logic.
func IsShutdownCancellation(ctx context.Context, err error) bool {
	return ctx != nil && ctx.Err() != nil && errors.Is(err, context.Canceled)
}

// transientOrShutdownStartError reports whether a fan-out attempt/child spawn
// error (best-of-n, parallel) should park the attempt as retryable ("pending")
// rather than permanently "failed". transientAgentStartError alone doesn't
// recognize context.Canceled, so a shutdown-cancelled spawn used to fall
// straight to a hard "failed" that finalizeBestOfNParent/finalizeParallelParent
// would then escalate the whole task to human-required for — the exact
// mass-park symptom sybra#2291 targets, reached through a code path the
// primary surfaceStartFailure fix doesn't gate on its own, since these two
// call sites only invoke surfaceStartFailure inside the transient branch.
func (e *Engine) transientOrShutdownStartError(err error) bool {
	return transientAgentStartError(err) || e.isShutdownCancellation(err)
}

func circuitBreakerFailureKey(stepID string) string { return circuitBreakerFailureVarPrefix + stepID }
func circuitBreakerFirstKey(stepID string) string   { return circuitBreakerFirstVarPrefix + stepID }

// recordCircuitBreakerFailure is the generic counterpart to
// handleWatchdogHangRetry's retry budget: instead of bounding retries of one
// specific known failure signature, it bounds ANY repeated agent-start
// failure for the same (task, step), regardless of cause. Once the count
// crosses maxCircuitBreakerFailures within circuitBreakerWindow, the caller
// trips the breaker.
//
// Deliberately not built on boundedRetry/rewindRetry: both cap a *count*
// against a max, but the breaker's cap is a sliding time window (a failure
// outside circuitBreakerWindow resets the counter instead of accumulating
// toward it), and it's invoked inline from surfaceStartFailureClassified
// with a *Execution/stepID/taskID rather than the TaskInfo+Step the other
// two helpers key off. Forcing it into either shape would mean threading the
// window-reset branch through a policy hook for one caller — more
// indirection than the four lines it shares with them are worth.
func recordCircuitBreakerFailure(wf *Execution, stepID string, now time.Time) (attempts int, trip bool) {
	firstKey := circuitBreakerFirstKey(stepID)
	failKey := circuitBreakerFailureKey(stepID)
	first, err := time.Parse(time.RFC3339, wf.Variables[firstKey])
	if err != nil || now.Sub(first) > circuitBreakerWindow {
		wf.SetVar(firstKey, now.Format(time.RFC3339))
		wf.SetVar(failKey, "1")
		return 1, false
	}
	attempts = parseWorkflowInt(wf.Variables[failKey]) + 1
	wf.SetVar(failKey, strconv.Itoa(attempts))
	return attempts, attempts >= maxCircuitBreakerFailures
}

// clearCircuitBreakerFailures drops the per-(task,step) failure counter once
// a dispatch attempt for that step succeeds, so a step that fails a couple of
// times, recovers, and later fails again starts a fresh window rather than
// inheriting stale attempts from an unrelated earlier incident.
func clearCircuitBreakerFailures(wf *Execution, stepID string) {
	if wf == nil || wf.Variables == nil || stepID == "" {
		return
	}
	delete(wf.Variables, circuitBreakerFailureKey(stepID))
	delete(wf.Variables, circuitBreakerFirstKey(stepID))
}

// clearCircuitBreakerOnSuccess persists the failure-counter reset performed
// by clearCircuitBreakerFailures. Split out so callers that don't have a
// failure counter to begin with (the common case) skip a wasted SetWorkflow.
func (e *Engine) clearCircuitBreakerOnSuccess(taskID string, wf *Execution, stepID string) {
	if wf == nil || wf.Variables == nil || stepID == "" {
		return
	}
	_, hasFail := wf.Variables[circuitBreakerFailureKey(stepID)]
	_, hasFirst := wf.Variables[circuitBreakerFirstKey(stepID)]
	if !hasFail && !hasFirst {
		return
	}
	clearCircuitBreakerFailures(wf, stepID)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		e.logger.Error("workflow.circuit-breaker.clear", "task_id", taskID, "step", stepID, "err", err)
	}
}
