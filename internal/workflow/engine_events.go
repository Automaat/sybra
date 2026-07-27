package workflow

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/dispatchorder"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/watchdogreason"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

const (
	watchdogHangStatusReasonPrefix  = "watchdog hang"
	watchdogHangRetryVarPrefix      = "watchdog.hang_retry."
	watchdogHangCleanRetryVarPrefix = "watchdog.hang_clean_retry."
	watchdogReaskNoteVar            = "watchdog_reask_note"
	maxWatchdogHangRetries          = 2
	watchdogStopRetryVarPrefix      = "watchdog.stop_retry."
	maxWatchdogStopRetries          = 2
	watchdogRateLimitStatusPrefix   = "watchdog: rate limit"
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
	// watchdogRewardHackingStatusPrefix must stay in sync with
	// internal/watchdog/agent.go's rewardHackingRetryStatusReason — the
	// watchdog writes this exact status-reason prefix when it retries a
	// reward_hacking stop on a fix-review agent (#2229), and
	// handleWatchdogRewardHackingRetry below pattern-matches on it.
	watchdogRewardHackingStatusPrefix   = "watchdog: reward-hacking retry"
	watchdogRewardHackingRetryVarPrefix = "watchdog.reward_hacking_retry."
	// maxWatchdogRewardHackingRetries is deliberately 1, not the generic hang
	// budget's 2: this path only fires when the review sidecar already names
	// the fix location, so a fresh agent that still can't land it after one
	// steered retry is a genuine stuck loop, not a flake.
	maxWatchdogRewardHackingRetries = 1
	transientFetchRetryVarPrefix    = "transient_fetch.retry."
	maxTransientFetchRetries        = 2
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

// HandleHumanAction processes approve/reject/input from the UI.
func (e *Engine) HandleHumanAction(taskID, action string, data map[string]string) error {
	return e.withHumanActionLock(taskID, func() error {
		return e.handleHumanAction(taskID, action, data)
	})
}

// HandleHumanActionRecovering processes a human action, optionally repairing a
// narrowly-recognized stale wait state before retrying the same action.
func (e *Engine) HandleHumanActionRecovering(
	taskID, action string,
	data map[string]string,
	recoverFn func(TaskInfo) (*Execution, bool, error),
) error {
	return e.withHumanActionLock(taskID, func() error {
		err := e.handleHumanAction(taskID, action, data)
		if err == nil || recoverFn == nil {
			return err
		}
		t, getErr := e.tasks.GetTask(taskID)
		if getErr != nil {
			return err
		}
		wf, ok, recoverErr := recoverFn(t)
		if recoverErr != nil {
			return recoverErr
		}
		if !ok {
			return err
		}
		if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
			return err
		}
		return e.handleHumanAction(taskID, action, data)
	})
}

// WithHumanActionLock runs fn under the per-task human-action lock, serializing
// it against concurrent human decisions on the same task — plan-review
// approvals via HandleHumanActionRecovering and operator dispatch from
// human-required all share this lock, so two humans (or a double-click) cannot
// race the same stuck task.
func (e *Engine) WithHumanActionLock(taskID string, fn func() error) error {
	return e.withHumanActionLock(taskID, fn)
}

func (e *Engine) withHumanActionLock(taskID string, fn func() error) error {
	// Serialize concurrent human actions per task so double-click races do not
	// both mutate workflow vars and attempt to advance the same wait_human step.
	e.mu.Lock()
	if _, busy := e.humanAction[taskID]; busy {
		e.mu.Unlock()
		return fmt.Errorf("task %s human action already in progress", taskID)
	}
	e.humanAction[taskID] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.humanAction, taskID)
		e.mu.Unlock()
	}()

	return fn()
}

func (e *Engine) handleHumanAction(taskID, action string, data map[string]string) error {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if t.Workflow != nil && t.Workflow.State == ExecWaiting && t.Workflow.CurrentStep != "" {
		def, err := e.store.Get(t.Workflow.WorkflowID)
		if err != nil {
			return err
		}
		currentStep := def.StepByID(t.Workflow.CurrentStep)
		if currentStep != nil && currentStep.Type == StepWaitHuman {
			if err := validateHumanAction(currentStep, action); err != nil {
				return err
			}
		}
	}
	if t.Workflow == nil || t.Workflow.State != ExecWaiting {
		return fmt.Errorf("task %s is not waiting for human action", taskID)
	}
	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return err
	}
	currentStep := def.StepByID(t.Workflow.CurrentStep)
	if currentStep == nil {
		return fmt.Errorf("step %s not found in workflow %s", t.Workflow.CurrentStep, def.ID)
	}
	if currentStep.Type != StepWaitHuman {
		recovered, rErr := e.recoverMissedWaitForStatusHumanGate(taskID, t, &def, currentStep, action)
		if rErr != nil {
			return rErr
		}
		if recovered {
			t, err = e.tasks.GetTask(taskID)
			if err != nil {
				return err
			}
			if t.Workflow == nil {
				return fmt.Errorf("task %s is not waiting for human action", taskID)
			}
			def, err = e.store.Get(t.Workflow.WorkflowID)
			if err != nil {
				return err
			}
			currentStep = def.StepByID(t.Workflow.CurrentStep)
			if currentStep == nil {
				return fmt.Errorf("step %s not found in workflow %s", t.Workflow.CurrentStep, def.ID)
			}
		}
	}
	if currentStep.Type != StepWaitHuman {
		return fmt.Errorf("task %s is not at a wait_human step", taskID)
	}
	if err := validateHumanAction(currentStep, action); err != nil {
		return err
	}

	wfExec := t.Workflow
	wfExec.SetVar("human_action", action)
	for k, v := range data {
		wfExec.SetVar("human."+k, v)
	}

	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}

	return e.AdvanceStep(taskID, StepOutput{
		StepID: wfExec.CurrentStep,
		Status: "completed",
		Output: action,
	})
}

func (e *Engine) recoverMissedWaitForStatusHumanGate(taskID string, t TaskInfo, def *Definition, step *Step, action string) (bool, error) {
	if t.Workflow == nil || step == nil || step.Type != StepRunAgent ||
		step.Config.WaitForStatus == "" || step.Config.WaitForStatus != t.Status {
		return false, nil
	}
	nextID, err := ResolveTransition(step.Next, e.transitionFields(t, t.Workflow))
	if err != nil {
		return false, err
	}
	if nextID == "" {
		return false, nil
	}
	nextStep := def.StepByID(nextID)
	if nextStep == nil {
		return false, fmt.Errorf("next step %s not found in workflow %s", nextID, def.ID)
	}
	if nextStep.Type != StepWaitHuman {
		return false, nil
	}
	if err := validateHumanAction(nextStep, action); err != nil {
		return false, err
	}
	e.logger.Info("workflow.human-action.recover-status",
		"task_id", taskID, "from", step.ID, "to", nextStep.ID, "status", t.Status)
	if err := e.AdvanceStep(taskID, StepOutput{
		StepID: step.ID,
		Status: "completed",
		Output: "recovered missed wait_for_status " + t.Status,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func validateHumanAction(step *Step, action string) error {
	if len(step.Config.HumanActions) > 0 && !slices.Contains(step.Config.HumanActions, action) {
		return fmt.Errorf("invalid human action %q for step %q", action, step.ID)
	}
	return nil
}

// HandleStatusChange is called when a task's status transitions. If the
// current workflow step is a run_agent configured with a matching
// wait_for_status, the workflow advances past it. This is how interactive /
// conversational agents (which don't exit between turns) signal step
// completion: they update the task status via the CLI, the task manager
// fires the status-change hook, and the engine advances the workflow.
//
// Safe to call for any status change — no-ops when the current step does
// not declare wait_for_status or when the status does not match.
func (e *Engine) HandleStatusChange(taskID, newStatus string) {
	if e.dispatchDisabled.Load() {
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Debug("workflow.status-change.get", "task_id", taskID, "err", err)
		return
	}
	if t.Workflow == nil || t.Workflow.CurrentStep == "" {
		return
	}
	if t.Workflow.State != ExecWaiting && t.Workflow.State != ExecRunning {
		return
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil || step.Type != StepRunAgent {
		return
	}
	// A task can self-escalate to human-required only because live verification
	// needs an open PR (branch already pushed) via a plain CLI status update,
	// without the agent exiting. That arrives here through the status hook, not
	// an agent-completion callback, so recover it before the wait_for_status
	// guard bails — otherwise the pushed branch strands in human-required.
	if comp, recovered, err := e.maybeRecoverHumanRequiredAlreadyFixedOnMain(taskID, step, t.Workflow, t, StepOutput{
		StepID: step.ID,
		Status: "completed",
		Output: t.StatusReason,
	}, t.StatusReason); recovered {
		if err != nil {
			e.logger.Error("workflow.status-recover.err", "task_id", taskID, "step", step.ID, "status", newStatus, "err", err)
			return
		}
		e.fireComplete(comp)
		return
	}
	if comp, recovered, err := e.maybeRecoverHumanRequiredByOpeningPR(taskID, step, t.Workflow, t, StepOutput{
		StepID: step.ID,
		Status: "completed",
		Output: t.StatusReason,
	}); recovered {
		if err != nil {
			e.logger.Error("workflow.status-recover.err", "task_id", taskID, "step", step.ID, "status", newStatus, "err", err)
			return
		}
		e.fireComplete(comp)
		return
	}
	if step.Config.WaitForStatus == "" || step.Config.WaitForStatus != newStatus {
		return
	}

	e.logger.Info("workflow.status-advance",
		"task_id", taskID, "step", step.ID, "status", newStatus)

	if err := e.AdvanceStep(taskID, StepOutput{
		StepID: step.ID,
		Status: "completed",
		Output: "status:" + newStatus,
	}); err != nil {
		e.logger.Error("workflow.status-advance.err", "task_id", taskID, "err", err)
	}
}

// HandleAgentComplete is called when an agent finishes. It maps the agent
// back to the workflow step and advances.
//
// Every non-advancing exit below logs at INFO with task_id, agent_id, and a
// reason — a completion that bails here produces no other signal (no error,
// no workflow.advance), so a Debug-only log made the drop invisible at the
// default log level and let a genuine regression (e.g. #1567) masquerade as
// routine "agent started outside the workflow engine" noise. Agents started
// outside the workflow engine (e.g. manual pr-fix retries, recovery spawns)
// legitimately land here on completion; the guards below avoid the "step not
// found" error loop that followed workflow completion in older versions —
// but that legitimacy still needs to be visible when diagnosing a stall.
func (e *Engine) HandleAgentComplete(taskID string, c AgentCompletion) {
	routeMu := e.taskRouteMutex(taskID)
	routeMu.Lock()
	e.mu.Lock()
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.mu.Unlock()
		routeMu.Unlock()
		e.logger.Error("workflow.agent-complete.get", "task_id", taskID, "err", err)
		return
	}
	spawnedStep, routeStatus := e.resolveCompletionRouteLocked(t, c)
	e.mu.Unlock()
	routeMu.Unlock()
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

	status := "completed"
	if !c.Success {
		status = "failed"
	}

	if c.Success {
		if def, ok := defs.get(); ok {
			e.importSidecarIfConfiguredFromDef(taskID, spawnedStep, t, def)
		} else {
			e.logger.Info("workflow.agent-complete.bail",
				"task_id", taskID, "agent_id", c.AgentID, "reason", "workflow-definition-unavailable", "current_step", spawnedStep)
		}
	}

	e.recordAgentCompletionTrace(taskID, spawnedStep, c, status)

	out := StepOutput{
		StepID:   spawnedStep,
		Status:   status,
		Output:   c.Result,
		AgentID:  c.AgentID,
		Provider: c.Provider,
	}
	if !c.Success && c.EscalationReason == "checkpoint_failed" {
		out.TerminalStatus = "human-required"
		out.TerminalReason = "checkpoint_failed: checkpoint commit failed — no durable checkpoint state created"
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
	def, err := c.engine.store.Get(c.task.Workflow.WorkflowID)
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

	def, err := e.store.Get(t.Workflow.WorkflowID)
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
	if e.shouldSkipResumeForRateLimitedProvider(&t, step) {
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

	def, err := e.store.Get(t.Workflow.WorkflowID)
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
	mu := e.taskInflightMutex(taskID)
	if !mu.TryLock() {
		e.logger.Debug(logPrefix+".skip",
			"task_id", taskID, "reason", "inflight", "step", step.ID)
		return
	}
	mu.Unlock()

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
			if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
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
	e.acquireInflight(taskID)
	defer e.releaseInflight(taskID)

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

	if e.shouldSkipResumeForRateLimitedProvider(&fresh, child) {
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
func resumeSkipReasonForStatus(status string) (reason string, skip bool) {
	switch status {
	case "human-required":
		return "human_required", true
	case "blocked":
		return "blocked", true
	case "done", "cancelled":
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
	// inflightMutexes is a non-blocking probe: TryLock distinguishes "another
	// goroutine currently holds the advance lock" from "free".
	mu := e.taskInflightMutex(taskID)
	advancing := !mu.TryLock()
	if !advancing {
		mu.Unlock()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	_, dispatching := e.dispatching[taskID]
	hasOutstandingAgent := false
	if fresh, err := e.tasks.GetTask(taskID); err == nil && fresh.Workflow != nil {
		for _, stepID := range fresh.Workflow.AgentRoutes {
			if stepID == step.ID || parallelHasChild(step, stepID) || bestOfNStepMatches(step, stepID) {
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
// definition no longer has.
func (e *Engine) escalateMissingStep(taskID string, wf *Execution) {
	e.logger.Warn("workflow.resume-stalled.step-missing",
		"task_id", taskID, "workflow_id", wf.WorkflowID, "step", wf.CurrentStep)

	// Status first, execution second, so a failed second write leaves the task
	// visible and retryable rather than buried mid-escalation.
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required",
		"Workflow step "+wf.CurrentStep+" no longer exists in "+wf.WorkflowID+
			" — it was removed while this task was parked on it. Set the task back to"+
			" planning to re-plan against the current workflow."); err != nil {
		e.logger.Warn("workflow.resume-stalled.step-missing.escalate", "task_id", taskID, "err", err)
		return
	}
	// Failing the execution is what makes the task recoverable: the planning
	// dispatcher only starts a fresh workflow when the old one is completed or
	// failed, so a waiting execution would reject the operator's re-plan.
	failed := *wf
	failed.State = ExecFailed
	if err := e.tasks.SetWorkflow(taskID, &failed); err != nil {
		e.logger.Warn("workflow.resume-stalled.step-missing.fail", "task_id", taskID, "err", err)
	}
}

// handleMissingStep applies the resumable path's own skip guards to a task
// whose current step no longer resolves, since a nil step never reaches them.
// A done/cancelled task needs no signal, and a live agent must keep its chance
// to land the sidecar — HandleAgentComplete bails on a terminal execution,
// so a failure here would discard the run.
//
// human-required is deliberately not skipped, unlike in the resumable path.
// escalateMissingStep writes the status before the execution, so an escalation
// whose second write failed sits at human-required with a live execution, which
// the planning dispatcher refuses to re-plan — the operator would be stuck
// following advice that cannot work. Re-entering here retries it. Nothing loops:
// once both writes land, ResumeStalled's own ExecFailed check skips the task
// before it ever reaches this function.
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
			return cmp.Compare(dispatchorder.Rank(a.Status), dispatchorder.Rank(b.Status))
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
	if retryAt, ok := workflowRetryAfter(t.Workflow); ok && time.Now().Before(retryAt) {
		retryAtStr := retryAt.Format(time.RFC3339)
		e.resumeSkip.Log(e.logger, logEvent, t.ID,
			"retry_after|"+step.ID+"|"+retryAtStr,
			"task_id", t.ID, "reason", "retry_after", "retry_after", retryAtStr, "step", step.ID)
		return true
	}
	retryableWatchdogStop := e.canRetryWatchdogStop(t, step)
	retryableWorktreeRepair := e.canRetryWorktreeRepair(t, step)
	if reason, skip := resumeSkipReasonForStatus(t.Status); skip &&
		(reason != "human_required" || !retryableWatchdogStop) &&
		(reason != "blocked" || !retryableWorktreeRepair) {
		e.resumeSkip.Log(e.logger, logEvent, t.ID,
			reason+"|"+t.Status+"|"+step.ID,
			"task_id", t.ID, "reason", reason, "status", t.Status, "step", step.ID)
		return true
	}
	if e.agents.HasRunningAgent(t.ID) {
		return true
	}
	if e.shouldSkipResumeForRateLimitedProvider(t, step) {
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
	if _, waitSkip := resumeSkipReasonForStatus(t.Status); step.Type == StepWaitHuman && !waitSkip && step.Config.Status != "" && t.Status != step.Config.Status {
		if err := e.tasks.UpdateTaskStatus(t.ID, step.Config.Status, step.Config.StatusReason); err != nil {
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

	wf := t.Workflow.Clone()
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
	e.clearWatchdogReaskNote(fresh.ID, cleanupWorkflow)
}

// handleWatchdogRetries checks both bounded watchdog stop-retry paths — a
// plain stall/generic-stall hang, and #2229's narrower
// reward-hacking-on-fix-review carve-out — and reports whether either
// consumed this tick, so ResumeStalled skips to the next task without
// dispatching.
func (e *Engine) handleWatchdogRetries(t *TaskInfo, step *Step) bool {
	if e.handleWatchdogHangRetry(t, step) {
		return true
	}
	return e.handleWatchdogRewardHackingRetry(t, step)
}

func (e *Engine) handleWatchdogHangRetry(t *TaskInfo, step *Step) bool {
	if !watchdogHangApplies(e, t, step) {
		return false
	}
	// A tracked agent for this task+step may still be mid-completion-routing
	// even though HasRunningAgent already returned false (see the persisted
	// route comment in ResumeStalled). Treating that window as a hang would burn
	// retry budget and clear the hang marker without a clean re-dispatch
	// actually happening.
	if e.hasTrackedAgentForTaskStep(t.ID, step.ID) {
		return false
	}
	if e.handleWatchdogHangReadyPR(t, step) {
		return true
	}
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name:       "watchdog-hang",
		applies:    watchdogHangApplies,
		busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
		counterKey: watchdogHangRetryKey,
		max:        maxWatchdogHangRetries,
		onArm: func(e *Engine, t *TaskInfo, step *Step, attempt int) {
			cleanRef := t.Workflow.Variables[tamperBaselineVar(step.ID)]
			if cleanRef == "" {
				cleanRef = "HEAD"
			}
			t.Workflow.SetVar(watchdogHangCleanRetryKey(step.ID), cleanRef)
			// ListTasks/taskToInfo never populates ManualTest; hydrate here so
			// the specialized run_test reask note carries the concrete
			// command/health/probe details instead of degrading to the
			// empty-surface fallback.
			t.Workflow.SetVar(watchdogReaskNoteVarForStep(step), buildWatchdogReaskNoteForStep(e.withManualTestConfig(*t), step, attempt))
		},
		onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
			return e.tasks.UpdateTaskStatus(t.ID, t.Status, "")
		},
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			targetStatus, reason, terminalState := watchdogHangExhaustionResolution(*t, step, attempts, e.openPROnUnrunnableGate)
			now := time.Now().UTC()
			t.Workflow.State = terminalState
			t.Workflow.CompletedAt = &now
			t.Workflow.CurrentStep = ""
			if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
				e.logger.Error("workflow.watchdog-hang.persist", "task_id", t.ID, "step", step.ID, "err", err)
			}
			if err := e.tasks.UpdateTaskStatus(t.ID, targetStatus, reason); err != nil {
				e.logger.Error("workflow.watchdog-hang.escalate", "task_id", t.ID, "step", step.ID, "err", err)
				return
			}
			if targetStatus == "ready-pr" {
				e.logger.Warn("workflow.watchdog-hang.exhausted.open-pr", "task_id", t.ID, "step", step.ID, "attempts", attempts)
				e.fireComplete(&CompletionInfo{
					TaskID:     t.ID,
					WorkflowID: t.Workflow.WorkflowID,
					Variables:  t.Workflow.Variables,
				})
				return
			}
			e.logger.Warn("workflow.watchdog-hang.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		},
	})
}

func watchdogHangApplies(_ *Engine, t *TaskInfo, step *Step) bool {
	return t != nil && t.Workflow != nil && step != nil && step.Type == StepRunAgent && isWatchdogHangReason(t.StatusReason)
}

func (e *Engine) handleWatchdogHangReadyPR(t *TaskInfo, step *Step) bool {
	if e.prStates == nil || t == nil || t.Workflow == nil || step == nil {
		return false
	}
	if t.ProjectID == "" || t.PRNumber <= 0 {
		return false
	}
	if t.Workflow.WorkflowID != "simple-task-implement" || step.ID != "implement" {
		return false
	}
	state, err := e.prStates.FetchPRState(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Warn("workflow.watchdog-hang.ready-pr.fetch", "task_id", t.ID, "pr", t.PRNumber, "err", err)
		return false
	}
	if !state.ReadyToMerge() {
		return false
	}

	delete(t.Workflow.Variables, watchdogReaskNoteVar)
	now := time.Now().UTC()
	t.Workflow.State = ExecCompleted
	t.Workflow.CompletedAt = &now
	t.Workflow.CurrentStep = ""
	t.Workflow.SetVar("cancel_reason", "watchdog hang: implementation superseded by linked PR already open and green")
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		e.logger.Error("workflow.watchdog-hang.ready-pr.persist", "task_id", t.ID, "step", step.ID, "pr", t.PRNumber, "err", err)
		return true
	}
	if err := e.tasks.UpdateTaskStatus(t.ID, "in-review", ""); err != nil {
		e.logger.Error("workflow.watchdog-hang.ready-pr.status", "task_id", t.ID, "step", step.ID, "pr", t.PRNumber, "err", err)
		return true
	}
	e.logger.Info("workflow.watchdog-hang.ready-pr", "task_id", t.ID, "step", step.ID, "pr", t.PRNumber, "ci_status", state.CIStatus())
	return true
}

func isWatchdogHangReason(reason string) bool {
	return watchdogreason.IsHang(reason)
}

func buildWatchdogReaskNote(attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous run on this step was TERMINATED because it produced no output "+
		"for an extended period (watchdog hang) — attempt %d of %d. A hang almost always means a "+
		"command blocked: the full test suite, a foreground server that never backgrounds, an "+
		"interactive prompt, or a wedged build.\n\n", attempt, maxWatchdogHangRetries)
	b.WriteString("To make forward progress this time:\n")
	b.WriteString("- Do NOT run the whole suite (`mise run verify`, `go test ./...`, full `npm` builds). " +
		"Sybra runs codegen and the verify suite deterministically AFTER you finish — build and test only " +
		"the narrow packages you changed.\n")
	b.WriteString("- Never launch a foreground long-running or interactive process; background any server " +
		"and bound every command.\n")
	b.WriteString("- Commit and push incrementally so partial progress survives a restart.\n")
	b.WriteString("- If you are genuinely blocked, STOP and mark the task human-required with the specific " +
		"blocker instead of looping.")
	return b.String()
}

// handleWatchdogRewardHackingRetry re-dispatches a fix-review step's agent
// once, fresh, when the watchdog stopped it for a reward_hacking pattern that
// it judged retriable (internal/watchdog/agent.go's
// retriableRewardHackingFixReview — a concrete, unaddressed review finding
// still exists to point the retry at). Bounded by its own dedicated budget
// (maxWatchdogRewardHackingRetries), separate from the generic hang budget,
// since this is a narrower and more targeted retry than a plain no-output
// hang. Exhausting it escalates to human-required, same as every other
// bounded watchdog retry.
func (e *Engine) handleWatchdogRewardHackingRetry(t *TaskInfo, step *Step) bool {
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name: "watchdog-reward-hacking",
		applies: func(_ *Engine, t *TaskInfo, step *Step) bool {
			return t != nil && t.Workflow != nil && step != nil && step.Type == StepRunAgent && isWatchdogRewardHackingReason(t.StatusReason)
		},
		// Mirrors handleWatchdogHangRetry's guard: a tracked agent for this
		// task+step may still be mid-completion-routing even though
		// HasRunningAgent already returned false.
		busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
		counterKey: watchdogRewardHackingRetryKey,
		max:        maxWatchdogRewardHackingRetries,
		onArm: func(_ *Engine, t *TaskInfo, step *Step, attempt int) {
			cleanRef := t.Workflow.Variables[tamperBaselineVar(step.ID)]
			if cleanRef == "" {
				cleanRef = "HEAD"
			}
			t.Workflow.SetVar(watchdogHangCleanRetryKey(step.ID), cleanRef)
			t.Workflow.SetVar(watchdogReaskNoteVar, buildRewardHackingReaskNote(attempt))
		},
		onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
			return e.tasks.UpdateTaskStatus(t.ID, t.Status, "")
		},
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			reason := fmt.Sprintf("watchdog: reward-hacking retry budget exhausted after %d clean re-dispatch(es) — review finding still unaddressed", attempts)
			t.Workflow.State = ExecFailed
			if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
				e.logger.Error("workflow.watchdog-reward-hacking.persist", "task_id", t.ID, "step", step.ID, "err", err)
			}
			if err := e.tasks.UpdateTaskStatus(t.ID, "human-required", reason); err != nil {
				e.logger.Error("workflow.watchdog-reward-hacking.escalate", "task_id", t.ID, "step", step.ID, "err", err)
				return
			}
			e.logger.Warn("workflow.watchdog-reward-hacking.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		},
	})
}

func isWatchdogRewardHackingReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == watchdogRewardHackingStatusPrefix || strings.HasPrefix(reason, watchdogRewardHackingStatusPrefix+":")
}

func watchdogRewardHackingRetryKey(stepID string) string {
	return watchdogRewardHackingRetryVarPrefix + stepID
}

// clearWatchdogRewardHackingRetry drops the per-step reward-hacking retry
// counter once that step's run completes cleanly. Without this, the same
// step ID (e.g. fix_review, which loops back through code_review each
// round) would carry an already-exhausted counter into a later, unrelated
// round and escalate to human-required on the very first reward_hacking stop
// of that round instead of retrying once as designed (#2229).
func clearWatchdogRewardHackingRetry(wf *Execution, stepID string) {
	if wf == nil || wf.Variables == nil || stepID == "" {
		return
	}
	delete(wf.Variables, watchdogRewardHackingRetryKey(stepID))
}

// buildRewardHackingReaskNote builds the steer prepended to a re-dispatched
// fix-review prompt: the previous attempt looped without editing anything, so
// point it straight at the finding the reviewer already located instead of
// re-reading unrelated files.
func buildRewardHackingReaskNote(attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous run on this step was TERMINATED because the watchdog detected a "+
		"reward-hacking pattern — repeating the same non-editing action (reading/navigating) instead of "+
		"making progress — attempt %d of %d.\n\n", attempt, maxWatchdogRewardHackingRetries)
	b.WriteString("The previous attempt stalled reading unrelated files; the code review sidecar already " +
		"names the fix location. Read it, then edit that exact file directly — do not re-read unrelated " +
		"files or repeat prior investigation.\n\n")
	b.WriteString("If you are genuinely blocked on understanding the finding, STOP and mark the task " +
		"human-required with the specific blocker instead of looping.")
	return b.String()
}

func buildWatchdogReaskNoteForStep(t TaskInfo, step *Step, attempt int) string {
	if !isTestRunnerWatchdogStep(step) {
		return buildWatchdogReaskNote(attempt)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous adversarial test run was TERMINATED for watchdog hang before it produced a verdict — attempt %d of %d.\n\n",
		attempt, maxWatchdogHangRetries)
	b.WriteString("Before any further repo reading, start the declared manual_test surface and prove it is live.\n")
	if t.ManualTest.Kind != "" {
		fmt.Fprintf(&b, "- manual_test kind: %s\n", t.ManualTest.Kind)
	}
	if cmd := strings.TrimSpace(t.ManualTest.Command); cmd != "" {
		fmt.Fprintf(&b, "- Start it first with: %s\n", cmd)
	}
	if url := strings.TrimSpace(t.ManualTest.HealthURL); url != "" {
		fmt.Fprintf(&b, "- Wait for health on: %s\n", url)
	}
	for _, probe := range t.ManualTest.ProbeCommands {
		if probe = strings.TrimSpace(probe); probe != "" {
			fmt.Fprintf(&b, "- Run probe: %s\n", probe)
		}
	}
	b.WriteString("- Do NOT spend another turn only reading implementation files before you have started the surface and captured at least one probe result.\n")
	b.WriteString("- Background long-running servers, bound every command, and avoid the full suite (`mise run verify`, `go test ./...`, full `npm` builds).\n")
	b.WriteString("- If the manual-testing surface itself is unrunnable, say exactly why in the final report instead of looping.\n")
	return b.String()
}

// watchdogReaskNoteVarForStep routes the hang-retry note to the workflow
// variable the step's own prompt consumes: the test-runner (run_test) prompt in
// testing-task.yaml reads testing_reask_note, while implementation prompts read
// watchdog_reask_note. Writing to the wrong channel means the guidance never
// reaches the retried agent (see the manual-test-surface note built for
// run_test hangs).
func watchdogReaskNoteVarForStep(step *Step) string {
	if isTestRunnerWatchdogStep(step) {
		return testingReaskNoteVar
	}
	return watchdogReaskNoteVar
}

func isTestRunnerWatchdogStep(step *Step) bool {
	if step == nil || step.Type != StepRunAgent {
		return false
	}
	return step.ID == testVerdictSourceStep || step.Config.Role == testRunnerRole
}

func watchdogHangExhaustionResolution(t TaskInfo, step *Step, attempts int, openPROnUnrunnableGate bool) (status, reason string, terminalState ExecState) {
	if t.Status == "testing" && isTestRunnerWatchdogStep(step) {
		if openPROnUnrunnableGate {
			return "ready-pr",
				"manual testing gate could not be run after auto-retries (harness/infra limitation, not a product defect) — opening PR for CI and human review",
				ExecCompleted
		}
		return "human-required",
			fmt.Sprintf("watchdog hang: run_test retry budget exhausted after %d clean re-dispatches", attempts),
			ExecFailed
	}
	return "human-required",
		fmt.Sprintf("watchdog hang: retry budget exhausted after %d clean re-dispatches", attempts),
		ExecFailed
}

func (e *Engine) handleWatchdogRateLimitRetry(t *TaskInfo, step *Step) bool {
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name: "watchdog-rate-limit",
		applies: func(_ *Engine, t *TaskInfo, step *Step) bool {
			return t != nil && t.Workflow != nil && step != nil && step.Type == StepRunAgent && isWatchdogRateLimitReason(t.StatusReason)
		},
		counterKey: watchdogRateLimitRetryKey,
		max:        maxWatchdogRateLimitRetries,
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			retryKey := watchdogRateLimitRetryKey(step.ID)
			freshKey := watchdogZeroOutputFreshRetryKey(step.ID)
			if watchdogreason.IsZeroOutputRateLimit(t.StatusReason) &&
				parseWorkflowInt(t.Workflow.Variables[freshKey]) == 0 {
				// Resume-attempt budget exhausted on a poisoned session. Fence it
				// (fresh dispatch next time), reset the budget, and retry once fresh
				// rather than parking blocked — see watchdogZeroOutputFreshRetryVarPrefix.
				t.Workflow.StartedAt = time.Now().UTC()
				t.Workflow.SetVar(freshKey, "1")
				t.Workflow.SetVar(retryKey, "0")
				if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
					e.logger.Error("workflow.watchdog-rate-limit.persist", "task_id", t.ID, "step", step.ID, "err", err)
					return
				}
				e.logger.Warn("workflow.watchdog-rate-limit.fresh-session-recovery",
					"task_id", t.ID, "step", step.ID, "resume_attempts", attempts+1)
				// Consume this tick: the reset budget + fence are now persisted, so the
				// next ResumeStalled dispatches a clean session from stored state
				// rather than resuming the poisoned one via the in-flight copy.
				return
			}
			targetStatus, reason, terminalState := watchdogRateLimitExhaustionResolution(*t, step, attempts)
			t.Workflow.State = terminalState
			if watchdogreason.IsZeroOutputRateLimit(t.StatusReason) {
				// Bump past PickImplementationResumeSession's StartedAt fence so the
				// next dispatch stops resuming this poisoned session. See sybra#2542.
				t.Workflow.StartedAt = time.Now().UTC()
			}
			if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
				e.logger.Error("workflow.watchdog-rate-limit.persist", "task_id", t.ID, "step", step.ID, "err", err)
			}
			var escalateErr error
			if targetStatus == "blocked" {
				// Zero-output startup exhaustion parks the task in the same
				// `blocked` status the umbrella dependency gate uses for a
				// gate-held child. Stamp a workflow-owned Blocker (mirrors
				// canRetryWorktreeRepair's KindWorktreeRepair) so
				// app_umbrella_gate.go's Awaiting check has an authoritative,
				// non-tag signal that this hold is not a dependency gate — a
				// child deep into implementation must never be mistaken for a
				// fresh, never-released one and re-released into a brand-new
				// triage cycle that discards its in-flight workflow (#2538).
				escalateErr = e.tasks.UpdateTaskBlocker(t.ID, targetStatus, reason, blocker.State{
					Kind:      blocker.KindWatchdogRateLimitExhausted,
					Actor:     blocker.ActorWorkflow,
					Exhausted: true,
				})
			} else {
				escalateErr = e.tasks.UpdateTaskStatus(t.ID, targetStatus, reason)
			}
			if escalateErr != nil {
				e.logger.Error("workflow.watchdog-rate-limit.escalate", "task_id", t.ID, "step", step.ID, "err", escalateErr)
				return
			}
			e.logger.Warn("workflow.watchdog-rate-limit.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts, "status", targetStatus)
		},
	})
}

func isWatchdogRateLimitReason(reason string) bool {
	return watchdogreason.IsRateLimit(reason)
}

func watchdogRateLimitExhaustionResolution(t TaskInfo, _ *Step, attempts int) (status, reason string, terminalState ExecState) {
	if watchdogreason.IsZeroOutputRateLimit(t.StatusReason) {
		return "blocked",
			fmt.Sprintf("watchdog: zero-output startup retry budget exhausted after %d identical attempts", attempts+1),
			ExecFailed
	}
	return "human-required",
		fmt.Sprintf("watchdog: rate limit retry budget exhausted after %d clean re-dispatches", attempts),
		ExecFailed
}

func (e *Engine) canRetryWatchdogStop(t *TaskInfo, step *Step) bool {
	return t != nil &&
		t.Workflow != nil &&
		step != nil &&
		step.Type == StepRunAgent &&
		t.Status == "human-required" &&
		step.Config.Role == "implementation" &&
		watchdogreason.IsRetryableStop(t.StatusReason)
}

func (e *Engine) handleWatchdogStopRetry(t *TaskInfo, step *Step) bool {
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name:    "watchdog-stop",
		applies: func(e *Engine, t *TaskInfo, step *Step) bool { return e.canRetryWatchdogStop(t, step) },
		// Same completion-routing race as watchdog hang retry: if the
		// just-stopped agent is still being routed, do not spend retry
		// budget or rewrite status.
		busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
		counterKey: watchdogStopRetryKey,
		max:        maxWatchdogStopRetries,
		onArm: func(_ *Engine, t *TaskInfo, step *Step, attempt int) {
			cleanRef := t.Workflow.Variables[tamperBaselineVar(step.ID)]
			if cleanRef == "" {
				cleanRef = "HEAD"
			}
			t.Workflow.SetVar(watchdogHangCleanRetryKey(step.ID), cleanRef)
			t.Workflow.SetVar(watchdogReaskNoteVar, buildWatchdogStopReaskNote(t.StatusReason, attempt))
		},
		onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
			if err := e.tasks.UpdateTaskStatus(t.ID, "in-progress", ""); err != nil {
				return err
			}
			t.Status = "in-progress"
			t.StatusReason = ""
			return nil
		},
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			reason := fmt.Sprintf("watchdog stop: retry budget exhausted after %d clean re-dispatches", attempts)
			t.Workflow.State = ExecFailed
			if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
				e.logger.Error("workflow.watchdog-stop.persist", "task_id", t.ID, "step", step.ID, "err", err)
			}
			if err := e.tasks.UpdateTaskStatus(t.ID, "human-required", reason); err != nil {
				e.logger.Error("workflow.watchdog-stop.escalate", "task_id", t.ID, "step", step.ID, "err", err)
				return
			}
			e.logger.Warn("workflow.watchdog-stop.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		},
	})
}

// canRetryWorktreeRepair reports whether t is parked blocked on a machine-
// recoverable worktree_repair blocker (disk-space exhaustion or a failed
// rebase) that has not yet used up its automated retry budget. Gives
// ResumeStalled a bypass of resumeSkipReasonForStatus's blanket "blocked"
// skip — mirrors canRetryWatchdogStop's human-required bypass.
func (e *Engine) canRetryWorktreeRepair(t *TaskInfo, step *Step) bool {
	return t != nil &&
		t.Workflow != nil &&
		step != nil &&
		step.Type == StepRunAgent &&
		t.Status == "blocked" &&
		t.Blocker.Kind == blocker.KindWorktreeRepair &&
		!t.Blocker.Exhausted
}

// handleWorktreeRepairRetry re-attempts a blocked worktree_repair task, or
// permanently exhausts it once the retry budget is spent. Returns true when
// this tick has been fully handled (either the retry was dispatched via a
// status flip, or the task was marked Exhausted); false means the caller
// should fall through to normal resume handling.
func (e *Engine) handleWorktreeRepairRetry(t *TaskInfo, step *Step) bool {
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name:    "worktree-repair",
		applies: func(e *Engine, t *TaskInfo, step *Step) bool { return e.canRetryWorktreeRepair(t, step) },
		// Same completion-routing race guard as watchdog stop retry: don't
		// spend retry budget or rewrite status while a dispatch for this
		// step is still being tracked.
		busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
		counterKey: worktreeRepairRetryKey,
		max:        maxWorktreeRepairRetries,
		onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
			if err := e.tasks.UpdateTaskBlocker(t.ID, "in-progress", "", blocker.State{}); err != nil {
				return err
			}
			t.Status = "in-progress"
			t.StatusReason = ""
			t.Blocker = blocker.State{}
			return nil
		},
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			exhausted := t.Blocker
			exhausted.Exhausted = true
			reason := fmt.Sprintf("worktree repair: retry budget exhausted after %d attempts — manual repair required", attempts)
			if err := e.tasks.UpdateTaskBlocker(t.ID, "blocked", reason, exhausted); err != nil {
				e.logger.Error("workflow.worktree-repair.escalate", "task_id", t.ID, "step", step.ID, "err", err)
				return
			}
			e.logger.Warn("workflow.worktree-repair.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		},
	})
}

func buildWatchdogStopReaskNote(reason string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous implementation run was STOPPED by the watchdog for loop-like behavior — attempt %d of %d.\n\n",
		attempt, maxWatchdogStopRetries)
	if reason = strings.TrimSpace(reason); reason != "" {
		b.WriteString("Previous watchdog reason: ")
		b.WriteString(reason)
		b.WriteString("\n\n")
	}
	b.WriteString("To make forward progress this time:\n")
	b.WriteString("- Do NOT repeat the same failing command or read-only investigation loop.\n")
	b.WriteString("- Inspect the latest concrete error/output, then change code or narrow the command before retrying.\n")
	b.WriteString("- If a deterministic check is failing, run only that narrow check and fix the root cause.\n")
	b.WriteString("- If a human genuinely must decide, stop and mark the task human-required with the exact blocker.")
	return b.String()
}

func (e *Engine) handleTransientFetchRetry(t *TaskInfo, step *Step) bool {
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name: "transient-fetch",
		applies: func(_ *Engine, t *TaskInfo, step *Step) bool {
			return t != nil && t.Workflow != nil && step != nil && step.Type == StepRunAgent && isTransientFetchReason(t.StatusReason)
		},
		counterKey: transientFetchRetryKey,
		max:        maxTransientFetchRetries,
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			reason := fmt.Sprintf("agent start blocked: transient network retry budget exhausted after %d attempts reconciling worktree with remote", attempts)
			if err := e.tasks.UpdateTaskStatus(t.ID, "human-required", reason); err != nil {
				e.logger.Error("workflow.transient-fetch.escalate", "task_id", t.ID, "step", step.ID, "err", err)
				return
			}
			e.logger.Warn("workflow.transient-fetch.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		},
	})
}

func (e *Engine) clearTransientFetchRetry(taskID string, wf *Execution, stepID string) {
	if wf == nil || wf.Variables == nil || stepID == "" {
		return
	}
	retryKey := transientFetchRetryKey(stepID)
	if _, ok := wf.Variables[retryKey]; !ok {
		return
	}
	delete(wf.Variables, retryKey)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		e.logger.Error("workflow.transient-fetch.clear", "task_id", taskID, "step", stepID, "err", err)
	}
}

func (e *Engine) clearWatchdogReaskNote(taskID string, wf *Execution) {
	if wf == nil || wf.Variables == nil {
		return
	}
	if _, ok := wf.Variables[watchdogReaskNoteVar]; !ok {
		return
	}
	delete(wf.Variables, watchdogReaskNoteVar)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		e.logger.Error("workflow.watchdog-hang.reask-clear", "task_id", taskID, "err", err)
	}
}

func watchdogHangRetryKey(stepID string) string {
	return watchdogHangRetryVarPrefix + stepID
}

func watchdogStopRetryKey(stepID string) string {
	return watchdogStopRetryVarPrefix + stepID
}

func watchdogHangCleanRetryKey(stepID string) string {
	return watchdogHangCleanRetryVarPrefix + stepID
}

func watchdogRateLimitRetryKey(stepID string) string {
	return watchdogRateLimitRetryVarPrefix + stepID
}

func watchdogZeroOutputFreshRetryKey(stepID string) string {
	return watchdogZeroOutputFreshRetryVarPrefix + stepID
}

func worktreeRepairRetryKey(stepID string) string {
	return worktreeRepairRetryVarPrefix + stepID
}

func transientFetchRetryKey(stepID string) string {
	return transientFetchRetryVarPrefix + stepID
}

func parseWorkflowInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (e *Engine) shouldSkipResumeForRateLimitedProvider(t *TaskInfo, step *Step) bool {
	// Don't re-dispatch a single-agent step to the same provider while it is
	// rate-limited; do continue when failover can route this run to a healthy
	// peer. Parallel children are checked at child spawn time because each child
	// can use a different provider.
	if step.Type != StepRunAgent {
		return false
	}
	prov := resolveProvider(step.Config.Provider, t.Workflow, e.agents.DefaultProvider(), *t)
	if !e.agents.ProviderRateLimited(prov) || e.agents.ProviderCanFailover(prov) {
		return false
	}
	e.logger.Debug("workflow.resume-stalled.skip",
		"task_id", t.ID, "reason", "provider_rate_limited", "provider", prov)
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
func (e *Engine) surfaceStartFailure(taskID, currentStatus string, err error, wf *Execution, stepID string) {
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
	if currentStatus != "human-required" && e.conflictRecovery != nil &&
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

func (e *Engine) surfaceStartFailureClassified(taskID, currentStatus string, err error, wf *Execution, stepID string) {
	failure := ClassifyAgentStartFailure(err)
	if failure.Reason == "" {
		return
	}
	// Sticky: a task already parked at human-required must not be touched
	// again from here. Without this, a call driven by a stale pre-dispatch
	// status snapshot could rewrite a status a concurrent handler resolved
	// to something more specific (e.g. in_review) back to human-required.
	if currentStatus == "human-required" {
		return
	}
	target := currentStatus
	if failure.Permanent {
		target = "human-required"
		if !failure.Blocker.IsZero() && !blocker.AllowsHumanRequired(failure.Blocker.Kind) {
			target = "blocked"
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
			target = "human-required"
			if !failure.Blocker.IsZero() && !blocker.AllowsHumanRequired(failure.Blocker.Kind) {
				target = "blocked"
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
