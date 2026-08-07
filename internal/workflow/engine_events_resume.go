package workflow

import (
	"cmp"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/dispatchorder"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/watchdogreason"
)

func resumeSkipReasonForStatus(status taskstatus.Status) (reason string, skip bool) {
	switch status {
	case taskstatus.HumanRequired:
		return "human_required", true
	case taskstatus.Blocked:
		return string(taskstatus.Blocked), true
	case taskstatus.Done, taskstatus.Cancelled:
		return "terminal_status", true
	default:
		return "", false
	}
}

func isResumableStepType(t StepType) bool {
	switch t {
	case StepRunAgent, StepParallel, StepBestOfN, StepClassifyTask, StepVerifyChecks, StepCreatePR, StepPushBranch, StepPromoteBestOfN, StepAdmissionPreflight:
		return true
	default:
		return false
	}
}

func (e *Engine) tryMarkResumeDispatching(taskID string, step *Step) (reason string, ok bool) {
	// Skip tasks whose step is currently being started. Interactive spawns
	// (worktree creation, rebase, agent process start) take several seconds
	// during which no agent is yet registered — without this guard the ticker
	// would spawn a duplicate and the second agent's completion would corrupt
	// the workflow at the wait_human gate.
	// inflightLocks is a non-blocking probe: TryLock distinguishes "another
	// goroutine currently holds the advance lock" from "free".
	probeUnlock, idle := e.inflightLocks.TryLockLocal(taskID)
	advancing := !idle
	if !advancing {
		probeUnlock()
	}

	// Retire orphaned routes before interpreting them as "agent still pending"
	// or this task wedges forever on a dead completion path.
	if !advancing {
		e.pruneStaleAgentRoutes(taskID, step)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	_, dispatching := e.dispatching[taskID]
	hasOutstandingAgent := false
	if fresh, err := e.tasks.GetTask(taskID); err == nil && fresh.Workflow != nil {
		for _, stepID := range fresh.Workflow.AgentRoutes {
			if routeMatchesStep(step, stepID) {
				hasOutstandingAgent = true
				break
			}
		}
	}

	// Also consult the shared agent.Manager dispatch-claim coordinator: a
	// claim it holds for taskID (e.g. recovery.RestartStaleInProgress mid
	// launch) is invisible to this engine's completion-routing bookkeeping,
	// and without this check ResumeStalled could start another run
	// concurrently with it.
	claimedElsewhere := e.agents.IsDispatching(taskID)

	if !advancing && !dispatching && !hasOutstandingAgent && !claimedElsewhere {
		e.dispatching[taskID] = struct{}{}
		return "", true
	}

	switch {
	case advancing:
		return "inflight", false
	case dispatching:
		return "dispatching", false
	case hasOutstandingAgent:
		return "agent-pending-completion", false
	case claimedElsewhere:
		return "claimed-elsewhere", false
	default:
		return "busy", false
	}
}

func (e *Engine) shouldSkipResumeAfterFreshRead(taskID string, wf *Execution) (TaskInfo, bool) {
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil || fresh.Workflow == nil {
		return fresh, true
	}
	if fresh.Workflow.CurrentStep != wf.CurrentStep || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed {
		return fresh, true
	}
	_, skip := resumeSkipReasonForStatus(fresh.Status)
	return fresh, skip
}

// ResumeStalled finds tasks with running/waiting workflows where no agent
// is active, and attempts to re-execute the current step.
// escalateMissingStep surfaces a task parked on a step its workflow no longer
// declares — typically a step deleted by a newer build while the task sat on
// it. Every advance path bails on a nil step, so without this the task strands
// forever with no operator signal and approve/reject hard-error.
//
// The execution is failed, not just flagged: ResumeStalled reaches this branch
// before its human-required skip, so leaving the execution waiting would
// re-escalate (and rewrite the task file) on every maintenance tick. Failing it
// also unblocks the operator, since approve/reject cannot resolve a step the
// definition no longer has. Status + workflow must land atomically: a partial
// write strands the task at human-required with a still-waiting execution that
// the planning dispatcher refuses to re-plan over.
func (e *Engine) escalateMissingStep(taskID string, wf *Execution) {
	e.logger.Warn("workflow.resume-stalled.step-missing",
		"task_id", taskID, "workflow_id", wf.WorkflowID, "step", wf.CurrentStep)

	reason := "Workflow step " + wf.CurrentStep + " no longer exists in " + wf.WorkflowID +
		" — it was removed while this task was parked on it. Set the task back to" +
		" planning to re-plan against the current workflow."
	failed := *wf
	failed.State = ExecFailed
	escalation := autonomy.NewEscalation("workflow.step_removed", autonomy.FailureOwnerOperatorDecision, autonomy.ProvenanceControlPlane, reason)
	if err := e.tasks.SetEscalationAndWorkflow(taskID, string(taskstatus.HumanRequired), reason, escalation, autonomy.OutcomeHumanRequired, &failed); err != nil {
		e.logger.Warn("workflow.resume-stalled.step-missing.escalate", "task_id", taskID, "err", err)
		return
	}
}

// handleMissingStep applies the resumable path's own skip guards to a task
// whose current step no longer resolves, since a nil step never reaches them.
// A done/cancelled task needs no signal, and a live agent must keep its chance
// to land the sidecar — HandleAgentComplete bails on a terminal execution,
// so a failure here would discard the run.
//
// human-required is deliberately not skipped, unlike in the resumable path.
// A failed atomic escalation leaves the task unchanged, so re-entering here on
// the next tick is the intended retry path. Nothing loops: once both writes
// land, ResumeStalled's own ExecFailed check skips the task before it ever
// reaches this function.
func (e *Engine) handleMissingStep(t *TaskInfo) {
	if reason, skip := resumeSkipReasonForStatus(t.Status); skip && reason != "human_required" {
		return
	}
	if e.agents.HasRunningAgent(t.ID) {
		return
	}
	e.escalateMissingStep(t.ID, t.Workflow)
}

func (e *Engine) ResumeStalled() {
	if e.dispatchDisabled.Load() {
		return
	}
	// Prune stale admission-queue items (missing/terminal/in-progress tasks)
	// before scanning, so this tick's ordering never reasons about a queued
	// item that no longer reflects live task state.
	if e.queueReconciler != nil {
		e.queueReconciler()
	}

	tasks, err := e.tasks.ListTasks()
	if err != nil {
		e.logger.Error("workflow.resume-stalled.list", "err", err)
		return
	}

	if e.dispatchComparator != nil {
		slices.SortStableFunc(tasks, e.dispatchComparator())
	} else {
		slices.SortStableFunc(tasks, func(a, b TaskInfo) int {
			return cmp.Compare(dispatchorder.Rank(string(a.Status)), dispatchorder.Rank(string(b.Status)))
		})
	}

	for i := range tasks {
		e.resumeStalledTask(&tasks[i])
	}
}

func (e *Engine) resumeStalledTask(t *TaskInfo) {
	if t.Workflow == nil || t.Workflow.CurrentStep == "" {
		return
	}
	if e.dispatchGate != nil && !e.dispatchGate(*t) {
		return
	}
	switch t.Workflow.State {
	case ExecCompleted, ExecFailed:
		return
	case ExecRunning, ExecWaiting:
		// fall through to resume logic
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		e.handleMissingStep(t)
		return
	}
	e.resumeStalledReconcileWaitHumanStatus(*t, step)
	if e.resumeStalledRerouteStaleConditionBranch(t, &def, step) {
		return
	}

	if !isResumableStepType(step.Type) {
		return
	}
	if e.resumePreflightConsumesTick(t, step, "workflow.resume-stalled.skip") {
		return
	}
	reason, acquired := e.tryMarkResumeDispatching(t.ID, step)
	if !acquired {
		e.resumeSkip.Log(e.logger, "workflow.resume-stalled.skip", t.ID,
			reason+"|"+step.ID,
			"task_id", t.ID, "reason", reason, "step", step.ID)
		return
	}

	// The resume won its claim, so every skip reason logged for this task is
	// over. Without this the throttle never re-arms and a LATER park is logged
	// nowhere for thirty minutes — worse than the per-tick Debug it replaced.
	// Clearing on a merely-passing preflight is wrong: the claim can still be
	// lost below, and that re-armed an INFO line on every single tick.
	e.resumeSkip.Clear(t.ID)

	// handleTransientFetchRetry runs only after the resume preflight passes,
	// so the retry budget tracks actual restart attempts —
	// a tick that loses the claim to a concurrent goroutine (already
	// claimed/advancing) never burns budget for a retry that didn't
	// happen.
	if e.handleTransientFetchRetry(t, step) {
		e.clearResumeDispatching(t.ID)
		return
	}

	fresh, abort := e.resolveFreshTaskForResume(t, step, &def)
	if abort {
		return
	}
	if e.handleWatchdogRateLimitRetry(&fresh, step) {
		e.clearResumeDispatching(t.ID)
		return
	}

	e.finishResumeStalledStep(t.ID, &def, step, fresh.Workflow, fresh)
}

func (e *Engine) resumePreflightConsumesTick(t *TaskInfo, step *Step, logEvent string) bool {
	if retryAt, ok := workflowRetryAfter(t.Workflow); ok && e.now().Before(retryAt) {
		retryAtStr := retryAt.Format(time.RFC3339)
		e.resumeSkip.Log(e.logger, logEvent, t.ID,
			"retry_after|"+step.ID+"|"+retryAtStr,
			"task_id", t.ID, "reason", "retry_after", "retry_after", retryAtStr, "step", step.ID)
		return true
	}
	if e.terminalizeNonRetryableRewardHacking(t, step) {
		return true
	}
	retryableWatchdogStop := e.canRetryWatchdogStop(t, step)
	retryableWorktreeRepair := e.canRetryWorktreeRepair(t, step)
	if reason, skip := resumeSkipReasonForStatus(t.Status); skip &&
		(reason != "human_required" || !retryableWatchdogStop) &&
		(reason != string(taskstatus.Blocked) || !retryableWorktreeRepair) {
		e.resumeSkip.Log(e.logger, logEvent, t.ID,
			reason+"|"+string(t.Status)+"|"+step.ID,
			"task_id", t.ID, "reason", reason, "status", t.Status, "step", step.ID)
		return true
	}
	if e.agents.HasRunningAgent(t.ID) {
		return true
	}
	if e.shouldSkipResumeForRateLimitedProvider(t, step, logEvent) {
		return true
	}
	if retryableWatchdogStop && e.handleWatchdogStopRetry(t, step) {
		return true
	}
	if retryableWorktreeRepair && e.handleWorktreeRepairRetry(t, step) {
		return true
	}
	if e.handleWatchdogRetries(t, step) {
		return true
	}
	return false
}

// terminalizeNonRetryableRewardHacking repairs the cross-component invariant
// between watchdog and workflow state. The watchdog parks unsupported roles
// at human-required because retrying the same ungrounded context is unsafe;
// leaving the workflow waiting at the same time makes guarded operator
// dispatch impossible, because a new workflow may only replace a terminal one.
func (e *Engine) terminalizeNonRetryableRewardHacking(t *TaskInfo, step *Step) bool {
	if t == nil || t.Workflow == nil || t.Status != taskstatus.HumanRequired ||
		!watchdogreason.IsRewardHacking(t.StatusReason) {
		return false
	}

	failed := t.Workflow.Clone()
	if failed == nil {
		return false
	}
	failed.State = ExecFailed
	now := time.Now().UTC()
	failed.CompletedAt = &now
	applied, err := e.tasks.SetWorkflowIf(t.ID, WorkflowWriteFence{
		Generation:   t.Generation,
		Status:       t.Status,
		StatusReason: t.StatusReason,
		WorkflowID:   t.Workflow.WorkflowID,
		CurrentStep:  t.Workflow.CurrentStep,
		State:        t.Workflow.State,
	}, failed)
	if err != nil {
		e.logger.Error("workflow.watchdog-reward-hacking.terminalize",
			"task_id", t.ID, "step", step.ID, "err", err)
		return true
	}
	if !applied {
		e.logger.Info("workflow.watchdog-reward-hacking.terminalize-stale",
			"task_id", t.ID, "step", step.ID)
		return true
	}
	t.Workflow = failed
	e.logger.Warn("workflow.watchdog-reward-hacking.terminalized",
		"task_id", t.ID, "step", step.ID)
	return true
}

// resolveFreshTaskForResume re-reads the task to guard against stale snapshots
// from concurrent ResumeStalled calls: by the time we pass the preflight, a
// prior goroutine may have already advanced the workflow past this step. It
// clears the resume-dispatching claim itself whenever it returns abort=true.
func (e *Engine) resolveFreshTaskForResume(t *TaskInfo, step *Step, def *Definition) (TaskInfo, bool) {
	fresh, skip := e.shouldSkipResumeAfterFreshRead(t.ID, t.Workflow)
	if skip {
		e.clearResumeDispatching(t.ID)
		return fresh, true
	}
	return fresh, false
}

func (e *Engine) resumeStalledReconcileWaitHumanStatus(t TaskInfo, step *Step) {
	if _, waitSkip := resumeSkipReasonForStatus(t.Status); step.Type == StepWaitHuman && !waitSkip && step.Config.Status != "" && t.Status != taskstatus.Status(step.Config.Status) {
		if err := e.tasks.UpdateTaskStatus(t.ID, taskstatus.Status(step.Config.Status), step.Config.StatusReason); err != nil {
			e.logger.Warn("workflow.resume-stalled.reconcile-status", "task_id", t.ID, "step", step.ID, "err", err)
		} else {
			e.logger.Info("workflow.resume-stalled.reconcile-status",
				"task_id", t.ID, "step", step.ID, "from", t.Status, "to", step.Config.Status)
		}
	}
}

func (e *Engine) resumeStalledRerouteStaleConditionBranch(t *TaskInfo, def *Definition, step *Step) bool {
	if t == nil || t.Workflow == nil || step == nil || e.agents.HasRunningAgent(t.ID) {
		return false
	}
	if _, skip := resumeSkipReasonForStatus(t.Status); skip {
		return false
	}
	condition := latestConditionPredecessor(def, t.Workflow, step.ID)
	if condition == nil {
		return false
	}
	nextID, err := ResolveTransition(condition.Next, e.transitionFields(*t, t.Workflow))
	if err != nil {
		e.logger.Warn("workflow.resume-stalled.condition-reroute.transition",
			"task_id", t.ID, "condition", condition.ID, "step", step.ID, "err", err)
		return false
	}
	if nextID == step.ID {
		return false
	}
	reason, acquired := e.tryMarkResumeDispatching(t.ID, step)
	if !acquired {
		e.resumeSkip.Log(e.logger, "workflow.resume-stalled.condition-reroute.skip", t.ID,
			reason+"|"+step.ID,
			"task_id", t.ID, "reason", reason, "step", step.ID)
		return true
	}
	defer e.clearResumeDispatching(t.ID)

	wf := t.Workflow.Clone()
	if wf == nil {
		e.logger.Warn("workflow.resume-stalled.condition-reroute.clone",
			"task_id", t.ID, "condition", condition.ID, "step", step.ID)
		return true
	}
	wf.CurrentStep = condition.ID
	wf.State = ExecRunning
	wf.CompletedAt = nil
	wf.ClearStepRecords(condition.ID)
	wf.ClearStepRecords(step.ID)
	if err := e.tasks.SetWorkflow(t.ID, wf); err != nil {
		e.logger.Warn("workflow.resume-stalled.condition-reroute.persist",
			"task_id", t.ID, "condition", condition.ID, "step", step.ID, "err", err)
		return true
	}

	e.logger.Info("workflow.resume-stalled.condition-reroute",
		"task_id", t.ID, "condition", condition.ID, "from", step.ID, "to", nextID)
	comp, rErr := e.executeSteps(t.ID, def, condition, wf)
	rErr = normalizeExecuteStepsErr(rErr)
	e.fireComplete(comp)
	e.resumeError.Log(e.logger, "workflow.resume-stalled.condition-reroute.exec", t.ID, rErr, "task_id", t.ID)
	if rErr != nil {
		e.surfaceStartFailure(t.ID, t.Status, rErr, wf, condition.ID)
	}
	return true
}

func latestConditionPredecessor(def *Definition, wf *Execution, currentStepID string) *Step {
	if def == nil || wf == nil || len(wf.StepHistory) == 0 || currentStepID == "" {
		return nil
	}
	last := wf.StepHistory[len(wf.StepHistory)-1]
	step := def.StepByID(last.StepID)
	if step == nil || step.Type != StepCondition {
		return nil
	}
	for i := range step.Next {
		if step.Next[i].GoTo == currentStepID {
			return step
		}
	}
	return nil
}

func (e *Engine) finishResumeStalledStep(taskID string, def *Definition, step *Step, wf *Execution, fresh TaskInfo) {
	e.logger.Info("workflow.resume-stalled", "task_id", taskID, "step", step.ID)
	metrics.OrchestratorResumeStalledFallback(e.metricContext())
	comp, rErr := e.executeSteps(taskID, def, step, wf)
	rErr = normalizeExecuteStepsErr(rErr)
	e.clearResumeDispatching(taskID)
	// Most resumable steps dispatch async work and return nil; sync retry steps
	// such as verify_checks can finish the workflow here.
	e.fireComplete(comp)
	e.drainPendingConflictRecovery(taskID)
	e.resumeError.Log(e.logger, "workflow.resume-stalled.exec", taskID, rErr, "task_id", taskID)
	if rErr != nil {
		e.surfaceStartFailure(taskID, fresh.Status, rErr, fresh.Workflow, step.ID)
		return
	}
	cleanupWorkflow := e.workflowForPostDispatchCleanup(taskID)
	e.clearTransientFetchRetry(fresh.ID, cleanupWorkflow, step.ID)
	e.clearCircuitBreakerOnSuccess(fresh.ID, cleanupWorkflow, step.ID)
	e.clearDeliveredWatchdogReaskNote(fresh.ID, step, cleanupWorkflow)
}

// handleWatchdogRetries checks both bounded watchdog stop-retry paths — a
// plain stall/generic-stall hang, and #2229's narrower
// reward-hacking-on-fix-review carve-out — and reports whether either
// consumed this tick, so ResumeStalled skips to the next task without
