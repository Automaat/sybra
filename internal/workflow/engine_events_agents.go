package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func (e *Engine) HandleAgentComplete(taskID string, c AgentCompletion) {
	// Held through routing + advance so stale-route pruning cannot retire this
	// task's still-legitimate routes mid-completion.
	defer e.enterCompletion(taskID)()

	unlockRoute := e.routeLocks.LockLocal(taskID)
	e.mu.Lock()
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.mu.Unlock()
		unlockRoute()
		e.logger.Error("workflow.agent-complete.get", "task_id", taskID, "err", err)
		return
	}
	spawnedStep, routeStatus := e.resolveCompletionRouteLocked(t, c)
	e.mu.Unlock()
	unlockRoute()
	if e.handleAgentCompleteInitialBail(taskID, t, c) {
		e.clearAgentStep(taskID, c.AgentID)
		return
	}

	// Resolve the step this agent was actually spawned for. For untracked
	// agents (post-restart recovery or manually-dispatched), fall back to the
	// workflow's current step — but only when no tracked agent is already in
	// flight for that task+step. If one is, this is a phantom completion (e.g.
	// a manual implementation agent completing during a code_review step) and
	// must be dropped to prevent it from advancing the wrong step.
	//
	defs := completionDefinitionCache{engine: e, task: t}
	switch routeStatus {
	case taskStepTracked:
		// c.AgentID owns spawnedStep. Falls through to the advancement logic
		// below exactly as the pre-#2176 tracked path always did.
	case taskStepRouted:
		e.logger.Info("workflow.agent-complete.bail",
			"task_id", taskID, "agent_id", c.AgentID, "reason", "untracked-ignored", "current_step", spawnedStep)
		e.clearAgentStep(taskID, c.AgentID)
		return
	case taskStepPending:
		if def, ok := defs.get(); ok {
			runRole := agentRunRole(t.AgentRuns, c.AgentID)
			if !pendingEffectCompletionMatchesCurrentStep(def, spawnedStep, runRole) {
				e.logger.Info("workflow.agent-complete.bail",
					"task_id", taskID, "agent_id", c.AgentID, "reason", "pending-effect-role-mismatch", "current_step", spawnedStep)
				e.clearAgentStep(taskID, c.AgentID)
				return
			}
		}
	case taskStepFree:
		// Neither a route nor a pending step effect exists for this task+step, so
		// the role match below is what decides.
		if runRole := agentRunRole(t.AgentRuns, c.AgentID); runRole != "" {
			if def, ok := defs.get(); ok && !untrackedCompletionMatchesCurrentStep(def, spawnedStep, runRole) {
				e.logger.Info("workflow.agent-complete.bail",
					"task_id", taskID, "agent_id", c.AgentID, "reason", "untracked-role-mismatch", "current_step", spawnedStep)
				e.clearAgentStep(taskID, c.AgentID)
				return
			}
		}
	}

	// A run_agent step declaring wait_for_status only completes on a matching
	// HandleStatusChange (see the wait_for_status branch below) — the step's
	// own agent finishing a turn
	// must never independently satisfy it. Interactive agents used to
	// guarantee this for free by never exiting mid-conversation, so this
	// callback simply never fired while a status match was pending; a
	// steerable headless run finalizes on its first completed turn
	// (drainOrCloseHeadlessSteer), so the equivalent guard has to be explicit
	// here now.
	if c.Success {
		if def, ok := defs.get(); ok {
			if s := def.StepByID(spawnedStep); s != nil && s.Config.WaitForStatus != "" {
				e.logger.Info("workflow.agent-complete.wait-for-status",
					"task_id", taskID, "agent_id", c.AgentID, "step", spawnedStep, "wait_for_status", s.Config.WaitForStatus)
				e.clearAgentStep(taskID, c.AgentID)
				return
			}
		}
	}

	if !c.Success && c.EscalationReason == "infrastructure_admission_deferred" {
		// Only a still-owned dispatch can be deferred. A duplicate terminal
		// event after route retirement must not re-arm the replacement effect.
		if routeStatus == taskStepTracked || routeStatus == taskStepPending {
			e.deferWorkerAdmission(taskID, c.AgentID)
		}
		return
	}

	status := e.importOrAdoptSidecarStatus(taskID, spawnedStep, t, c, &defs)

	e.recordAgentCompletionTrace(taskID, spawnedStep, c, status)

	out := StepOutput{
		StepID:   spawnedStep,
		Status:   status,
		Output:   c.Result,
		AgentID:  c.AgentID,
		Provider: c.Provider,
	}
	if !c.Success && c.EscalationReason == "checkpoint_failed" {
		out.TerminalStatus = taskstatus.HumanRequired
		out.TerminalReason = "checkpoint_failed: checkpoint commit failed — no durable checkpoint state created"
	}
	if !c.Success && c.EscalationReason == "permanent_execution_failure" {
		out.TerminalStatus = taskstatus.Blocked
		out.TerminalReason = "remote execution was rejected permanently; operator action is required before retry"
	}
	if def, ok := defs.get(); ok && e.maybeRecoverUnverifiedSkillRun(taskID, c.AgentID, spawnedStep, c.Result, def, def.StepByID(t.Workflow.CurrentStep)) {
		e.clearAgentStep(taskID, c.AgentID)
		return
	}

	if err := e.AdvanceStep(taskID, out); err != nil {
		e.logger.Error("workflow.agent-complete.advance", "task_id", taskID, "err", err)
		// Same surfacing as ResumeStalled: AdvanceStep often fails because
		// the *next* step couldn't spawn its agent (e.g. project missing).
		// Without this, the task sits in-progress with the only signal in
		// logs until ResumeStalled gets a chance to retry.
		//
		// The failure is attributed to the step that actually failed to
		// dispatch (unwrapped from a dispatchStepError), not spawnedStep —
		// AdvanceStep usually fails advancing *past* spawnedStep, into the
		// next step, so keying circuit-breaker/reason bookkeeping off
		// spawnedStep would blame the wrong step and let the real offender's
		// stale counters leak into unrelated future chains.
		if t.Status != "" {
			failedStep := dispatchFailureStepID(err, spawnedStep)
			e.surfaceStartFailure(taskID, t.Status, err, t.Workflow, failedStep)
		}
	}
	e.clearAgentStep(taskID, c.AgentID)
}

// importOrAdoptSidecarStatus resolves the step status a completed agent run
// should be recorded with. A successful run imports its sidecars and
// completes normally. A failed run gets one more chance: if it still left a
// complete, valid set of sidecar artifacts on disk (e.g. aborted_streaming
// after all plan files were saved), adopting them turns the step into a
// completed one instead of burning a retry attempt re-doing already-finished
// work.
func (e *Engine) importOrAdoptSidecarStatus(taskID, spawnedStep string, t TaskInfo, c AgentCompletion, defs *completionDefinitionCache) string {
	if c.Success {
		if def, ok := defs.get(); ok {
			e.importSidecarIfConfiguredFromDef(taskID, spawnedStep, t, def)
		} else {
			e.logger.Info("workflow.agent-complete.bail",
				"task_id", taskID, "agent_id", c.AgentID, "reason", "workflow-definition-unavailable", "current_step", spawnedStep)
		}
		return "completed"
	}
	if def, ok := defs.get(); ok && e.adoptSidecarsFromFailedRun(taskID, spawnedStep, t, def) {
		return "completed"
	}
	return "failed"
}

func (e *Engine) handleAgentCompleteInitialBail(taskID string, t TaskInfo, c AgentCompletion) bool {
	if t.Workflow == nil {
		e.logger.Info("workflow.agent-complete.bail", "task_id", taskID, "agent_id", c.AgentID, "reason", "no-workflow")
		return true
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		// Still import sidecar for tracked agents that finish after the
		// workflow turns terminal (e.g. an untracked agent advanced the step
		// first, leaving the real agent's output file unread).
		if c.Success {
			if spawnedStep, tracked := e.lookupAgentStep(taskID, c.AgentID); tracked {
				e.importSidecarIfConfigured(taskID, spawnedStep, t)
			}
		}
		e.logger.Info("workflow.agent-complete.bail",
			"task_id", taskID, "agent_id", c.AgentID, "reason", "terminal", "state", string(t.Workflow.State))
		return true
	}
	if t.Workflow.CurrentStep == "" {
		e.logger.Info("workflow.agent-complete.bail",
			"task_id", taskID, "agent_id", c.AgentID, "reason", "no-current-step", "state", string(t.Workflow.State))
		return true
	}
	return false
}

func (e *Engine) recordAgentCompletionTrace(taskID, spawnedStep string, c AgentCompletion, status string) {
	if e.recorder == nil {
		return
	}
	tid := traceID(taskID, spawnedStep, c.AgentID)
	ev := map[string]any{
		"trace_id": tid,
		"traceId":  tid,
		"task_id":  taskID,
		"taskId":   taskID,
		"step":     spawnedStep,
		"status":   status,
		"agent_id": c.AgentID,
		"agentId":  c.AgentID,
		"provider": c.Provider,
	}
	if recErr := e.recorder.RecordTrace(taskID, ev); recErr != nil {
		e.logger.Warn("artifact.record.failed", "kind", "trace", "task_id", taskID, "step", spawnedStep, "err", recErr)
	}
}

func traceID(taskID, stepID, agentID string) string {
	sum := sha256.Sum256([]byte(taskID + "|" + stepID + "|" + agentID + "|"))
	return "trace-" + hex.EncodeToString(sum[:])[:12]
}

// hasTrackedAgentForTaskStep returns true when a tracked agent already owns the
// given task+step pair, or when that step's async action effect is still
// pending and the route has not been durably completed yet.
func (e *Engine) hasTrackedAgentForTaskStep(taskID, stepID string) bool {
	if taskID == "" || stepID == "" {
		return false
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return false
	}
	for _, routedStep := range t.Workflow.AgentRoutes {
		if routedStep == stepID {
			return true
		}
	}
	return routeStepPending(t, stepID)
}

// taskStepStatus is resolveCompletionRoute's verdict for an agent completion.
type taskStepStatus int

const (
	// taskStepFree means no route and no pending effect — HandleAgentComplete
	// falls through to its other untracked-completion handling.
	taskStepFree taskStepStatus = iota
	// taskStepTracked means c.AgentID itself has a registered route — the
	// normal, common case: deliver as a tracked completion.
	taskStepTracked
	// taskStepRouted means some other persisted route already owns this
	// task+step — that agent legitimately owns it, so the completion is a
	// phantom.
	taskStepRouted
	// taskStepPending means the current step still owns a pending action effect,
	// but no durable route was published for this agent ID.
	taskStepPending
)

// resolveCompletionRouteLocked resolves one completion against the workflow's
// persisted agent routes plus the current step's pending effect intent. Caller
// holds e.mu while reading the route tables.
func (e *Engine) resolveCompletionRouteLocked(t TaskInfo, c AgentCompletion) (spawnedStep string, status taskStepStatus) {
	if t.Workflow == nil {
		return "", taskStepFree
	}
	if stepID, ok := t.Workflow.AgentRoute(c.AgentID); ok {
		return stepID, taskStepTracked
	}
	if stepID, ok := e.pendingRoutes[pendingAgentRouteKey(t.ID, c.AgentID)]; ok {
		return stepID, taskStepTracked
	}
	spawnedStep = t.Workflow.CurrentStep
	if stepID, ok := parallelChildStepByAgentID(t.Workflow, spawnedStep, c.AgentID); ok {
		return stepID, taskStepTracked
	}
	if stepID, ok := bestOfNAttemptStepByAgentID(t.Workflow, spawnedStep, c.AgentID); ok {
		return stepID, taskStepTracked
	}
	for agentID, stepID := range t.Workflow.AgentRoutes {
		if agentID != c.AgentID && stepID == spawnedStep {
			return spawnedStep, taskStepRouted
		}
	}
	if routeStepPending(t, spawnedStep) {
		return spawnedStep, taskStepPending
	}
	return spawnedStep, taskStepFree
}

type completionDefinitionCache struct {
	engine *Engine
	task   TaskInfo
	def    *Definition
}

func (c *completionDefinitionCache) get() (*Definition, bool) {
	if c.def != nil {
		return c.def, true
	}
	if c.task.Workflow == nil || c.task.Workflow.WorkflowID == "" {
		return nil, false
	}
	def, err := c.engine.resolveExecutionDefinition(c.task.ID, c.task)
	if err != nil {
		return nil, false
	}
	c.def = &def
	return c.def, true
}

func agentRunRole(runs []AgentRunInfo, agentID string) string {
	if agentID == "" {
		return ""
	}
	for i := range slices.Backward(runs) {
		if runs[i].AgentID == agentID {
			return runs[i].Role
		}
	}
	return ""
}

func untrackedCompletionMatchesCurrentStep(def *Definition, stepID, runRole string) bool {
	if def == nil || stepID == "" || runRole == "" {
		return true
	}
	step := def.StepByID(stepID)
	if step == nil || step.Type != StepRunAgent || step.Config.Role == "" {
		return true
	}
	return step.Config.Role == runRole
}

func pendingEffectCompletionMatchesCurrentStep(def *Definition, stepID, runRole string) bool {
	if def == nil || stepID == "" {
		return false
	}
	step := def.StepByID(stepID)
	if step == nil || step.Type != StepRunAgent {
		return false
	}
	if step.Config.Role == "" || runRole == "" {
		return true
	}
	return step.Config.Role == runRole
}

// ClearAgentStep removes the persisted agent→step mapping without advancing the
// workflow.
// Used when an agent exits due to an infrastructure-level signal kill so the
// tracked-agent entry is released while the workflow step stays stalled for
// ResumeStalled to re-dispatch.
func (e *Engine) ClearAgentStep(taskID, agentID string) {
	e.clearAgentStep(taskID, agentID)
}

// RescheduleInterruptedAgent re-drives a run_agent step interrupted by a
// rejected tool use. Some providers surface that interruption as
// human-required before completion routing runs, so this path may narrowly
// unpark the task when the interrupted agent is still the workflow's tracked
// agent for the current step.
func (e *Engine) RescheduleInterruptedAgent(taskID, agentID string) {
	if taskID == "" {
		e.clearAgentStep(taskID, agentID)
		e.logger.Warn("workflow.interrupted-reschedule.untracked", "agent_id", agentID)
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || t.Workflow.CurrentStep == "" {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if reason, skip := resumeSkipReasonForStatus(t.Status); skip && reason != "human_required" {
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(t.Workflow, agentID)
		if err := e.tasks.SetWorkflow(taskID, t.Workflow); err != nil {
			e.logger.Error("workflow.interrupted-reschedule.clear-route", "task_id", taskID, "agent_id", agentID, "reason", reason, "err", err)
		}
		return
	}
	def, err := e.resolveExecutionDefinition(taskID, t)
	if err != nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil || step.Type != StepRunAgent {
		e.clearAgentStep(taskID, agentID)
		return
	}
	spawnedStep, tracked := e.lookupAgentStep(taskID, agentID)
	if !tracked || spawnedStep != step.ID {
		e.clearAgentStep(taskID, agentID)
		return
	}
	e.clearAgentStep(taskID, agentID)
	clearAgentRouteFromWorkflow(t.Workflow, agentID)
	if t.Status == taskstatus.HumanRequired {
		status := interruptedRecoveryStatus(t.Workflow.WorkflowID)
		if err := e.tasks.UpdateTaskStatus(taskID, status, ""); err != nil {
			e.logger.Error("workflow.interrupted-reschedule.unpark", "task_id", taskID, "step", step.ID, "err", err)
			return
		}
		t.Status = status
		t.StatusReason = ""
	}
	e.rescheduleRunAgent(taskID, agentID, step, t, &def, "workflow.interrupted-reschedule", nil)
}

func interruptedRecoveryStatus(workflowID string) taskstatus.Status {
	if workflowID == "simple-task-plan" {
		return taskstatus.Planning
	}
	if workflowID == "testing-task" {
		return taskstatus.Testing
	}
	return taskstatus.InProgress
}

// RescheduleRateLimitedAgent immediately re-drives the run_agent step that a
// rate-limited agent was executing. It excludes the completing agent from the
// running-agent check because headless done closes only after onComplete returns.
func (e *Engine) RescheduleRateLimitedAgent(taskID, agentID string) {
	if taskID == "" {
		e.clearAgentStep(taskID, agentID)
		e.logger.Warn("workflow.rate-limit-reschedule.untracked", "agent_id", agentID)
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || t.Workflow.CurrentStep == "" {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if _, skip := resumeSkipReasonForStatus(t.Status); t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed || skip {
		e.clearAgentStep(taskID, agentID)
		return
	}

	def, err := e.resolveExecutionDefinition(taskID, t)
	if err != nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	spawnedStep, tracked := e.lookupAgentStep(taskID, agentID)
	if step.Type == StepParallel {
		if !tracked || !parallelHasChild(step, spawnedStep) {
			e.clearAgentStep(taskID, agentID)
			return
		}
		child := def.StepByID(spawnedStep)
		if child == nil || child.Type != StepRunAgent {
			e.clearAgentStep(taskID, agentID)
			return
		}
		e.rescheduleRateLimitedParallelChild(taskID, agentID, step, child, t)
		return
	}
	if step.Type != StepRunAgent {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if tracked && spawnedStep != step.ID {
		e.clearAgentStep(taskID, agentID)
		return
	}
	e.clearAgentStep(taskID, agentID)
	clearAgentRouteFromWorkflow(t.Workflow, agentID)
	e.rescheduleRateLimitedRunAgent(taskID, agentID, step, t, &def)
}

func (e *Engine) rescheduleRateLimitedRunAgent(taskID, agentID string, step *Step, t TaskInfo, def *Definition) {
	e.markSilentHangProvider(&t, step, agentID)
	if e.shouldSkipResumeForRateLimitedProvider(&t, step, "workflow.rate-limit-reschedule.park") {
		// Provider is still inside its rate-limit cooldown and no healthy peer is
		// available to fail over to. Park the task without consuming a watchdog
		// retry budget or feeding the circuit breaker — ResumeStalled re-drives
		// it once clearExpiredRateLimits reopens a provider.
		return
	}
	e.rescheduleRunAgent(taskID, agentID, step, t, def, "workflow.rate-limit-reschedule", func(t *TaskInfo, step *Step) bool {
		return e.handleWatchdogRateLimitRetry(t, step)
	})
}

func (e *Engine) tryMarkRescheduleDispatching(taskID string, step *Step, logPrefix string) bool {
	e.mu.Lock()
	if _, dispatching := e.dispatching[taskID]; dispatching {
		e.mu.Unlock()
		e.logger.Debug(logPrefix+".skip",
			"task_id", taskID, "reason", "dispatching", "step", step.ID)
		return false
	}
	e.dispatching[taskID] = struct{}{}
	e.mu.Unlock()
	return true
}

func (e *Engine) clearResumeDispatching(taskID string) {
	e.mu.Lock()
	delete(e.dispatching, taskID)
	e.mu.Unlock()
}

func (e *Engine) resumeDispatching(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.dispatching[taskID]
	return ok
}

// RescheduleCheckpointedAgent immediately re-drives the current run_agent step
// after a durable turn-ceiling checkpoint handoff, bounded by a persisted
// per-step checkpoint counter.
func (e *Engine) RescheduleCheckpointedAgent(taskID, agentID string) {
	if taskID == "" {
		e.clearAgentStep(taskID, agentID)
		e.logger.Warn("workflow.checkpoint-reschedule.untracked", "agent_id", agentID)
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || t.Workflow.CurrentStep == "" {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if _, skip := resumeSkipReasonForStatus(t.Status); t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed || skip {
		e.clearAgentStep(taskID, agentID)
		return
	}

	def, err := e.resolveExecutionDefinition(taskID, t)
	if err != nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	// best_of_n / parallel attempts keep the parent block as CurrentStep, so a
	// checkpointed attempt is a child of the block, not a plain run_agent step.
	// We can't synchronously re-drive a single attempt here (the block executors
	// own attempt lifecycle), but we must still charge the checkpoint against
	// the per-step budget so an attempt can't checkpoint-loop past
	// agent.max_checkpoints — ResumeStalled re-enters the block resume-safely to
	// respawn the pending attempt.
	if step.Type == StepBestOfN || step.Type == StepParallel {
		spawnedStep, tracked := e.lookupAgentStep(taskID, agentID)
		isChild := (step.Type == StepParallel && parallelHasChild(step, spawnedStep)) ||
			(step.Type == StepBestOfN && bestOfNStepMatches(step, spawnedStep))
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(t.Workflow, agentID)
		if !tracked || !isChild {
			return
		}
		// Enforces MaxCheckpoints and escalates to human-required on exhaustion;
		// the return value only gates the synchronous run_agent re-drive, which
		// does not apply to block attempts, so it is intentionally ignored.
		e.handleCheckpointReschedule(taskID, &t, step)
		return
	}
	if step.Type != StepRunAgent {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if spawnedStep, tracked := e.lookupAgentStep(taskID, agentID); tracked && spawnedStep != step.ID {
		e.clearAgentStep(taskID, agentID)
		return
	}

	e.clearAgentStep(taskID, agentID)
	clearAgentRouteFromWorkflow(t.Workflow, agentID)
	e.rescheduleRunAgent(taskID, agentID, step, t, &def, "workflow.checkpoint-reschedule", func(t *TaskInfo, step *Step) bool {
		return e.handleCheckpointReschedule(taskID, t, step)
	})
}

func (e *Engine) rescheduleRunAgent(taskID, agentID string, step *Step, t TaskInfo, def *Definition, logPrefix string, beforeDispatch func(*TaskInfo, *Step) bool) {
	if e.agents.HasOtherRunningAgentForTask(taskID, agentID) {
		e.logger.Debug(logPrefix+".skip",
			"task_id", taskID, "reason", "other-agent-running", "step", step.ID)
		return
	}
	probeUnlock, ok := e.inflightLocks.TryLockLocal(taskID)
	if !ok {
		e.logger.Debug(logPrefix+".skip",
			"task_id", taskID, "reason", "inflight", "step", step.ID)
		return
	}
	probeUnlock()

	if !e.tryMarkRescheduleDispatching(taskID, step, logPrefix) {
		return
	}
	defer e.clearResumeDispatching(taskID)

	if e.agents.IsDispatching(taskID) {
		e.logger.Debug(logPrefix+".skip",
			"task_id", taskID, "reason", "claim-held", "step", step.ID)
		return
	}

	if beforeDispatch != nil && beforeDispatch(&t, step) {
		return
	}

	// This route logs its own park via shouldSkipResumeForRateLimitedProvider
	// (beforeDispatch above) and fires before ResumeStalled ever sees the task,
	// so it needs its own re-arm: without it a second park here is dropped
	// entirely rather than logged.
	e.resumeSkip.Clear(taskID)

	e.logger.Info(logPrefix, "task_id", taskID, "step", step.ID)
	comp, rErr := e.executeSteps(taskID, def, step, t.Workflow)
	rErr = normalizeExecuteStepsErr(rErr)
	if rErr == nil && e.shouldRetryGhostPark(taskID, step.ID) {
		e.logger.Info(logPrefix+".retry", "task_id", taskID, "step", step.ID)
		comp, rErr = e.executeSteps(taskID, def, step, t.Workflow)
		rErr = normalizeExecuteStepsErr(rErr)
	}
	e.fireComplete(comp)
	e.drainPendingConflictRecovery(taskID)
	e.resumeError.Log(e.logger, logPrefix+".exec", taskID, rErr, "task_id", taskID)
	if rErr != nil {
		e.surfaceStartFailure(taskID, t.Status, rErr, t.Workflow, step.ID)
		return
	}
	cleanupWorkflow := e.workflowForPostDispatchCleanup(taskID)
	e.clearCircuitBreakerOnSuccess(taskID, cleanupWorkflow, step.ID)
	e.clearWatchdogReaskNote(taskID, cleanupWorkflow)
}

func (e *Engine) workflowForPostDispatchCleanup(taskID string) *Execution {
	if taskID == "" {
		return nil
	}
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Error("workflow.post-dispatch-cleanup.get", "task_id", taskID, "err", err)
		return nil
	}
	return fresh.Workflow
}

func (e *Engine) shouldRetryGhostPark(taskID, stepID string) bool {
	if e.agents.HasRunningAgent(taskID) || e.agents.IsDispatching(taskID) {
		return false
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return false
	}
	for _, routedStep := range t.Workflow.AgentRoutes {
		if routedStep == stepID {
			return false
		}
	}
	return t.Workflow.State == ExecWaiting && t.Workflow.CurrentStep == stepID
}

func (e *Engine) checkpointRescheduleKey(stepID string) string {
	return "step." + stepID + ".checkpoint_count"
}

// maxPromptUndeliveredRetries bounds re-dispatches for runs whose prompt never
// reached the provider child. The stall lane skips HandleAgentComplete, so
// nothing else counts these — without a ceiling a wedged CLI would burn a
// two-minute write timeout per tick forever instead of reaching a human.
const maxPromptUndeliveredRetries = 3

func promptUndeliveredKey(stepID string) string {
	return "step." + stepID + ".prompt_undelivered"
}

func (e *Engine) handlePromptUndeliveredReschedule(taskID string, t *TaskInfo, step *Step) bool {
	if t.Workflow == nil {
		return true
	}
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name:       "prompt-undelivered-reschedule",
		applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
		counterKey: promptUndeliveredKey,
		max:        maxPromptUndeliveredRetries,
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			reason := fmt.Sprintf("provider never accepted the prompt across %d attempts — the agent CLI is not reading stdin on this host", attempts+1)
			if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); err != nil {
				e.resumeError.Log(e.logger, "workflow.prompt-undelivered-reschedule.human-required", taskID, err, "task_id", taskID)
			}
		},
	})
}

// ReschedulePromptUndeliveredAgent re-drives a run_agent step whose provider
// child never read the prompt, charging each attempt against a per-step budget
// so a persistently wedged CLI escalates instead of looping.
func (e *Engine) ReschedulePromptUndeliveredAgent(taskID, agentID string) {
	if taskID == "" {
		e.clearAgentStep(taskID, agentID)
		e.logger.Warn("workflow.prompt-undelivered-reschedule.untracked", "agent_id", agentID)
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || t.Workflow.CurrentStep == "" {
		e.clearAgentStep(taskID, agentID)
		return
	}
	if _, skip := resumeSkipReasonForStatus(t.Status); t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed || skip {
		e.clearAgentStep(taskID, agentID)
		return
	}
	def, err := e.resolveExecutionDefinition(taskID, t)
	if err != nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	spawnedStep, tracked := e.lookupAgentStep(taskID, agentID)
	if step.Type != StepRunAgent || (tracked && spawnedStep != step.ID) {
		// Block attempts still charge the budget so a wedged CLI cannot loop
		// inside a parallel/best-of-n child; ResumeStalled respawns the attempt.
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(t.Workflow, agentID)
		if tracked {
			e.handlePromptUndeliveredReschedule(taskID, &t, step)
		}
		return
	}
	e.clearAgentStep(taskID, agentID)
	clearAgentRouteFromWorkflow(t.Workflow, agentID)
	e.rescheduleRunAgent(taskID, agentID, step, t, &def, "workflow.prompt-undelivered-reschedule", func(t *TaskInfo, step *Step) bool {
		return e.handlePromptUndeliveredReschedule(taskID, t, step)
	})
}

func (e *Engine) effectiveMaxCheckpoints() int {
	if e.maxCheckpoints > 0 {
		return e.maxCheckpoints
	}
	return defaultMaxCheckpoints
}

func (e *Engine) handleCheckpointReschedule(taskID string, t *TaskInfo, step *Step) bool {
	if t.Workflow == nil {
		return true
	}
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name:       "checkpoint-reschedule",
		applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
		counterKey: e.checkpointRescheduleKey,
		max:        e.effectiveMaxCheckpoints(),
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			reason := fmt.Sprintf("checkpoint retry budget exhausted after %d handoffs", e.effectiveMaxCheckpoints())
			if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); err != nil {
				e.resumeError.Log(e.logger, "workflow.checkpoint-reschedule.human-required", taskID, err, "task_id", taskID)
			}
		},
		onPersistError: func(e *Engine, t *TaskInfo, step *Step, err error) {
			e.resumeError.Log(e.logger, "workflow.checkpoint-reschedule.persist", taskID, err, "task_id", taskID)
			e.surfaceStartFailure(taskID, t.Status, err, t.Workflow, step.ID)
		},
	})
}

func (e *Engine) rescheduleRateLimitedParallelChild(taskID, agentID string, parent, child *Step, t TaskInfo) {
	unlockInflight := e.acquireInflight(taskID)
	defer unlockInflight()

	fresh, err := e.tasks.GetTask(taskID)
	_, skip := resumeSkipReasonForStatus(fresh.Status)
	if err != nil || fresh.Workflow == nil || fresh.Workflow.CurrentStep != parent.ID || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed || skip {
		e.clearAgentStep(taskID, agentID)
		return
	}
	wfExec := fresh.Workflow
	rec := wfExec.ParallelInflight[parent.ID]
	if rec == nil {
		e.clearAgentStep(taskID, agentID)
		return
	}
	status := rec.Children[child.ID]
	if status == nil || status.AgentID != agentID {
		e.clearAgentStep(taskID, agentID)
		return
	}
	e.clearAgentStep(taskID, agentID)
	clearAgentRouteFromWorkflow(wfExec, agentID)

	e.markSilentHangProvider(&fresh, child, agentID)
	if e.shouldSkipResumeForRateLimitedProvider(&fresh, child, "workflow.rate-limit-reschedule.park") {
		// See RescheduleRateLimitedAgent: park rather than burn a retry budget or
		// trip the breaker while this child's provider is rate-limited with no
		// failover peer. The parent stays inflight; ResumeStalled retries later.
		return
	}

	if e.handleWatchdogRateLimitRetry(&fresh, child) {
		return
	}

	status.Status = "pending"
	status.Output = "rate-limited: rescheduled"
	status.AgentID = ""

	fresh = e.withManualTestConfig(fresh)
	ctx := TemplateContext{
		Task:     fresh,
		Step:     *parent,
		Vars:     wfExec.Variables,
		Workflow: wfExec,
	}
	dir := wfExec.Variables[WorkflowVarDir]
	// This route never reaches rescheduleRunAgent, so it re-arms for itself.
	e.resumeSkip.Clear(taskID)
	e.logger.Info("workflow.rate-limit-reschedule.parallel",
		"task_id", taskID, "parent", parent.ID, "child", child.ID)
	spawnErr := e.spawnParallelChild(taskID, parent, child, wfExec, ctx, dir, status)
	if spawnErr != nil {
		status.Status = "pending"
		status.Output = "reschedule failed: " + spawnErr.Error()
		status.AgentID = ""
	} else {
		clearCircuitBreakerFailures(wfExec, child.ID)
	}
	if setErr := e.tasks.SetWorkflow(taskID, wfExec); setErr != nil {
		e.logger.Error("workflow.rate-limit-reschedule.parallel.set", "task_id", taskID, "parent", parent.ID, "child", child.ID, "err", setErr)
	}
	if spawnErr != nil {
		e.surfaceStartFailure(taskID, fresh.Status, spawnErr, wfExec, child.ID)
	}
}

// resumeSkipReasonForStatus reports whether ResumeStalled must not resume a
// task in the given status, and why.
//
// human-required/blocked: the task was halted by a competing path or parked
// for human/operator follow-up. Resuming would override that verdict and
// re-dispatch an agent the operator or workflow already suppressed.
//
// done/cancelled: the task reached a terminal status (e.g. its PR merged)
// while its Workflow record was still Running/Waiting from before that
// transition landed. Resuming would reprep the worktree and rebase an
// already-merged branch against origin/main, which self-conflicts and flips
// the task back to human-required. The cancel-on-landing path clears
// Workflow going forward; this guard covers any terminal status this ticker
// still finds stale.
