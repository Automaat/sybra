package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/worktreeerr"
)

const (
	watchdogHangStatusReasonPrefix  = "watchdog hang"
	watchdogHangRetryVarPrefix      = "watchdog.hang_retry."
	watchdogHangCleanRetryVarPrefix = "watchdog.hang_clean_retry."
	maxWatchdogHangRetries          = 2
	watchdogRateLimitStatusPrefix   = "watchdog: rate limit"
	watchdogRateLimitRetryVarPrefix = "watchdog.rate_limit_retry."
	maxWatchdogRateLimitRetries     = 2
	transientFetchRetryVarPrefix    = "transient_fetch.retry."
	maxTransientFetchRetries        = 2
	circuitBreakerFailureVarPrefix  = "circuit_breaker.failures."
	circuitBreakerFirstVarPrefix    = "circuit_breaker.first_failure."
	maxCircuitBreakerFailures       = 3
	circuitBreakerWindow            = 15 * time.Minute
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
	t, err = e.advanceSatisfiedWaitForStatus(taskID, t)
	if err != nil {
		return err
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
		return fmt.Errorf("task %s is not at a wait_human step", taskID)
	}
	if len(currentStep.Config.HumanActions) > 0 && !slices.Contains(currentStep.Config.HumanActions, action) {
		return fmt.Errorf("invalid human action %q for step %q", action, currentStep.ID)
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

// advanceSatisfiedWaitForStatus repairs the common "missed watcher event"
// state where a run_agent step is waiting for a status that the task already
// has. This lets human approve/reject clicks reconcile the workflow before
// validating that the current step is wait_human.
func (e *Engine) advanceSatisfiedWaitForStatus(taskID string, t TaskInfo) (TaskInfo, error) {
	if t.Workflow == nil || t.Workflow.CurrentStep == "" {
		return t, nil
	}
	if t.Workflow.State != ExecWaiting && t.Workflow.State != ExecRunning {
		return t, nil
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return t, err
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		return t, fmt.Errorf("step %s not found in workflow %s", t.Workflow.CurrentStep, def.ID)
	}
	if step.Type != StepRunAgent || step.Config.WaitForStatus == "" || step.Config.WaitForStatus != t.Status {
		return t, nil
	}

	e.logger.Info("workflow.wait-for-status.reconcile",
		"task_id", taskID, "step", step.ID, "status", t.Status)
	if err := e.AdvanceStep(taskID, StepOutput{
		StepID: step.ID,
		Status: "completed",
		Output: "status:" + t.Status,
	}); err != nil {
		return TaskInfo{}, err
	}
	return e.tasks.GetTask(taskID)
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
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Error("workflow.agent-complete.get", "task_id", taskID, "err", err)
		return
	}
	if t.Workflow == nil {
		e.logger.Info("workflow.agent-complete.bail", "task_id", taskID, "agent_id", c.AgentID, "reason", "no-workflow")
		return
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		// Still import sidecar for tracked agents that finish after the
		// workflow turns terminal (e.g. an untracked agent advanced the step
		// first, leaving the real agent's output file unread).
		if c.Success {
			if spawnedStep, tracked := e.lookupAgentStep(c.AgentID); tracked {
				e.importSidecarIfConfigured(taskID, spawnedStep, t)
			}
		}
		e.logger.Info("workflow.agent-complete.bail",
			"task_id", taskID, "agent_id", c.AgentID, "reason", "terminal", "state", string(t.Workflow.State))
		e.clearAgentStep(c.AgentID)
		return
	}
	if t.Workflow.CurrentStep == "" {
		e.logger.Info("workflow.agent-complete.bail",
			"task_id", taskID, "agent_id", c.AgentID, "reason", "no-current-step", "state", string(t.Workflow.State))
		e.clearAgentStep(c.AgentID)
		return
	}

	// Resolve the step this agent was actually spawned for. For untracked
	// agents (post-restart recovery or manually-dispatched), fall back to the
	// workflow's current step — but only when no tracked agent is already in
	// flight for that task+step. If one is, this is a phantom completion (e.g.
	// a manual implementation agent completing during a code_review step) and
	// must be dropped to prevent it from advancing the wrong step.
	spawnedStep, tracked := e.lookupAgentStep(c.AgentID)
	if !tracked {
		spawnedStep = t.Workflow.CurrentStep
		if e.hasTrackedAgentForTaskStep(taskID, spawnedStep) {
			e.logger.Info("workflow.agent-complete.bail",
				"task_id", taskID, "agent_id", c.AgentID, "reason", "untracked-ignored", "current_step", spawnedStep)
			e.clearAgentStep(c.AgentID)
			return
		}
	}

	status := "completed"
	if !c.Success {
		status = "failed"
	}

	if c.Success {
		e.importSidecarIfConfigured(taskID, spawnedStep, t)
	}

	if e.recorder != nil {
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

	if err := e.AdvanceStep(taskID, StepOutput{
		StepID:   spawnedStep,
		Status:   status,
		Output:   c.Result,
		AgentID:  c.AgentID,
		Provider: c.Provider,
	}); err != nil {
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
	e.clearAgentStep(c.AgentID)
}

func traceID(taskID, stepID, agentID string) string {
	sum := sha256.Sum256([]byte(taskID + "|" + stepID + "|" + agentID + "|"))
	return "trace-" + hex.EncodeToString(sum[:])[:12]
}

// lookupAgentStep returns the stepID an agent was spawned for and whether it
// was tracked. Untracked agents fall back to the workflow's current step.
func (e *Engine) lookupAgentStep(agentID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.agentSteps[agentID]
	return entry.stepID, ok
}

// hasTrackedAgentForTaskStep returns true when a tracked agent is already in
// flight for the given task+step pair, OR a run_agent dispatch for that pair
// is in progress but hasn't been assigned an agent ID yet (see
// dispatchingStep). Used to detect phantom completions from untracked
// (manually-dispatched, or reattached-and-stale) agents: without the
// dispatchingStep check, a stale completion arriving while the real agent for
// the current step is still being started (e.g. blocked on worktree prep)
// falls back to "current step, nothing tracked yet" and gets misattributed —
// advancing the step before its real agent ever ran.
func (e *Engine) hasTrackedAgentForTaskStep(taskID, stepID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, entry := range e.agentSteps {
		if entry.taskID == taskID && entry.stepID == stepID {
			return true
		}
	}
	return e.dispatchingStep[dispatchingStepKey(taskID, stepID)] > 0
}

func dispatchingStepKey(taskID, stepID string) string {
	return taskID + "|" + stepID
}

// markStepDispatching records that a run_agent step's agent-start sequence
// (which may block for seconds on worktree prep) is underway for taskID/stepID,
// before an agent ID exists to register in agentSteps. Paired with
// unmarkStepDispatching, always via defer, on every return path.
func (e *Engine) markStepDispatching(taskID, stepID string) {
	e.mu.Lock()
	key := dispatchingStepKey(taskID, stepID)
	e.dispatchingStep[key]++
	e.mu.Unlock()
}

func (e *Engine) unmarkStepDispatching(taskID, stepID string) {
	e.mu.Lock()
	key := dispatchingStepKey(taskID, stepID)
	if e.dispatchingStep[key] <= 1 {
		delete(e.dispatchingStep, key)
		e.mu.Unlock()
		return
	}
	e.dispatchingStep[key]--
	e.mu.Unlock()
}

// clearAgentStep removes the agent→step mapping. Safe to call for unknown IDs.
func (e *Engine) clearAgentStep(agentID string) {
	if agentID == "" {
		return
	}
	e.mu.Lock()
	delete(e.agentSteps, agentID)
	e.mu.Unlock()
}

// clearAgentStepsForTask drops every agent→step mapping owned by a task. Called
// right after StopAgentsForTask on a (re)dispatch: any agent we just stopped is
// superseded by the one about to be spawned, so its late or double-delivered
// completion must not be credited to the workflow. Clearing the entry turns
// that completion "untracked", at which point the phantom-completion guard in
// HandleAgentComplete drops it (the freshly-dispatched agent is the only tracked
// agent for the current step). Without this a stopped test-runner's late
// provider-error completion lands on the still-current run_test step and burns
// the retry budget before the retry agent has produced a verdict.
func (e *Engine) clearAgentStepsForTask(taskID string) {
	if taskID == "" {
		return
	}
	e.mu.Lock()
	for id, entry := range e.agentSteps {
		if entry.taskID == taskID {
			delete(e.agentSteps, id)
		}
	}
	e.mu.Unlock()
}

// ClearAgentStep removes the agent→step mapping without advancing the workflow.
// Used when an agent exits due to an infrastructure-level signal kill so the
// tracked-agent entry is released while the workflow step stays stalled for
// ResumeStalled to re-dispatch.
func (e *Engine) ClearAgentStep(agentID string) {
	e.clearAgentStep(agentID)
}

// RescheduleRateLimitedAgent immediately re-drives the run_agent step that a
// rate-limited agent was executing. It excludes the completing agent from the
// running-agent check because headless done closes only after onComplete returns.
func (e *Engine) RescheduleRateLimitedAgent(taskID, agentID string) {
	if taskID == "" {
		e.clearAgentStep(agentID)
		e.logger.Warn("workflow.rate-limit-reschedule.untracked", "agent_id", agentID)
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || t.Workflow.CurrentStep == "" {
		e.clearAgentStep(agentID)
		return
	}
	if _, skip := resumeSkipReasonForStatus(t.Status); t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed || skip {
		e.clearAgentStep(agentID)
		return
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		e.clearAgentStep(agentID)
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		e.clearAgentStep(agentID)
		return
	}
	spawnedStep, tracked := e.lookupAgentStep(agentID)
	if step.Type == StepParallel {
		if !tracked || !parallelHasChild(step, spawnedStep) {
			e.clearAgentStep(agentID)
			return
		}
		child := def.StepByID(spawnedStep)
		if child == nil || child.Type != StepRunAgent {
			e.clearAgentStep(agentID)
			return
		}
		e.rescheduleRateLimitedParallelChild(taskID, agentID, step, child, t)
		return
	}
	if step.Type != StepRunAgent {
		e.clearAgentStep(agentID)
		return
	}
	if tracked && spawnedStep != step.ID {
		e.clearAgentStep(agentID)
		return
	}
	e.clearAgentStep(agentID)
	if e.shouldSkipResumeForRateLimitedProvider(&t, step) {
		// Provider is still inside its rate-limit cooldown and no healthy peer is
		// available to fail over to. Park the task without consuming a watchdog
		// retry budget or feeding the circuit breaker — ResumeStalled re-drives
		// it once clearExpiredRateLimits reopens a provider.
		return
	}
	if e.agents.HasOtherRunningAgentForTask(taskID, agentID) {
		e.logger.Debug("workflow.rate-limit-reschedule.skip",
			"task_id", taskID, "reason", "other-agent-running", "step", step.ID)
		return
	}
	mu := e.taskInflightMutex(taskID)
	if !mu.TryLock() {
		e.logger.Debug("workflow.rate-limit-reschedule.skip",
			"task_id", taskID, "reason", "inflight", "step", step.ID)
		return
	}
	mu.Unlock()

	e.mu.Lock()
	if _, dispatching := e.dispatching[taskID]; dispatching {
		e.mu.Unlock()
		e.logger.Debug("workflow.rate-limit-reschedule.skip",
			"task_id", taskID, "reason", "dispatching", "step", step.ID)
		return
	}
	e.dispatching[taskID] = struct{}{}
	e.mu.Unlock()
	if e.handleWatchdogRateLimitRetry(&t, step) {
		e.mu.Lock()
		delete(e.dispatching, taskID)
		e.mu.Unlock()
		return
	}

	e.logger.Info("workflow.rate-limit-reschedule", "task_id", taskID, "step", step.ID)
	comp, rErr := e.executeSteps(taskID, &def, step, t.Workflow)
	e.mu.Lock()
	delete(e.dispatching, taskID)
	e.mu.Unlock()
	e.fireComplete(comp)
	e.drainPendingConflictRecovery(taskID)
	e.resumeError.Log(e.logger, "workflow.rate-limit-reschedule.exec", taskID, rErr, "task_id", taskID)
	if rErr != nil {
		e.surfaceStartFailure(taskID, t.Status, rErr, t.Workflow, step.ID)
	} else {
		e.clearCircuitBreakerOnSuccess(taskID, t.Workflow, step.ID)
	}
}

func (e *Engine) rescheduleRateLimitedParallelChild(taskID, agentID string, parent, child *Step, t TaskInfo) {
	e.acquireInflight(taskID)
	defer e.releaseInflight(taskID)

	fresh, err := e.tasks.GetTask(taskID)
	_, skip := resumeSkipReasonForStatus(fresh.Status)
	if err != nil || fresh.Workflow == nil || fresh.Workflow.CurrentStep != parent.ID || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed || skip {
		e.clearAgentStep(agentID)
		return
	}
	wfExec := fresh.Workflow
	rec := wfExec.ParallelInflight[parent.ID]
	if rec == nil {
		e.clearAgentStep(agentID)
		return
	}
	status := rec.Children[child.ID]
	if status == nil || status.AgentID != agentID {
		e.clearAgentStep(agentID)
		return
	}
	e.clearAgentStep(agentID)

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
// human-required: the task was halted by a competing path (e.g. the inline
// review triage deciding the PR is too small). Resuming would override the
// triage verdict and re-dispatch an agent the operator already suppressed.
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
	case "done", "cancelled":
		return "terminal_status", true
	default:
		return "", false
	}
}

func (e *Engine) tryMarkResumeDispatching(taskID string, step *Step) (reason string, acquired bool) {
	// Skip tasks whose step is currently being dispatched. Interactive spawns
	// (worktree creation, rebase, agent process start) take several seconds
	// during which no agent is yet registered — without this guard the ticker
	// would spawn a duplicate and the second agent's completion would corrupt
	// the workflow at the wait_human gate.
	// inflightMutexes is a non-blocking probe: TryLock distinguishes "another
	// goroutine currently holds the advance lock" from "free". We only set
	// dispatching when both the advance lock and prior dispatching guard are
	// free.
	mu := e.taskInflightMutex(taskID)
	advancing := !mu.TryLock()
	if !advancing {
		mu.Unlock()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	_, dispatching := e.dispatching[taskID]
	// agentSteps holds outstanding agents the engine spawned but hasn't yet
	// routed completion for. Required because interactive agents pass through
	// StatePaused after their first result event (one-shot path closes stdin →
	// state Paused → process exits → onComplete fires → AdvanceStep), and
	// HasRunningAgent returns false during that window. Without this check a
	// tight ResumeStalled loop dispatches a duplicate.
	hasOutstandingAgent := false
	for _, entry := range e.agentSteps {
		if entry.taskID == taskID && (entry.stepID == step.ID || parallelHasChild(step, entry.stepID) || bestOfNStepMatches(step, entry.stepID)) {
			hasOutstandingAgent = true
			break
		}
	}

	if !advancing && !dispatching && !hasOutstandingAgent {
		e.dispatching[taskID] = struct{}{}
		return "", true
	}

	switch {
	case advancing:
		return "inflight", false
	case hasOutstandingAgent:
		return "agent-pending-completion", false
	default:
		return "dispatching", false
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
func (e *Engine) ResumeStalled() {
	tasks, err := e.tasks.ListTasks()
	if err != nil {
		e.logger.Error("workflow.resume-stalled.list", "err", err)
		return
	}

	for i := range tasks {
		t := &tasks[i]
		if t.Workflow == nil || t.Workflow.CurrentStep == "" {
			continue
		}
		switch t.Workflow.State {
		case ExecCompleted, ExecFailed:
			continue
		case ExecRunning, ExecWaiting:
			// fall through to resume logic
		}

		def, dErr := e.store.Get(t.Workflow.WorkflowID)
		if dErr != nil {
			continue
		}
		step := def.StepByID(t.Workflow.CurrentStep)
		if step == nil {
			continue
		}

		// Only resume async agent steps where no agent is running, plus
		// classify_task: it's synchronous but can park in ExecWaiting on
		// engine shutdown mid-classify (see engine_steps_classify.go), and
		// nothing else re-drives a parked sync step.
		if step.Type != StepRunAgent && step.Type != StepParallel && step.Type != StepBestOfN && step.Type != StepClassifyTask {
			continue
		}
		if retryAt, ok := workflowRetryAfter(t.Workflow); ok && time.Now().Before(retryAt) {
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", "retry_after", "retry_after", retryAt.Format(time.RFC3339), "step", step.ID)
			continue
		}
		if reason, skip := resumeSkipReasonForStatus(t.Status); skip {
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", reason, "status", t.Status, "step", step.ID)
			continue
		}
		if e.agents.HasRunningAgent(t.ID) {
			continue
		}
		if e.shouldSkipResumeForRateLimitedProvider(t, step) {
			continue
		}
		if e.handleWatchdogHangRetry(t, step) {
			continue
		}
		reason, acquired := e.tryMarkResumeDispatching(t.ID, step)
		if !acquired {
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", reason, "step", step.ID)
			continue
		}

		// handleTransientFetchRetry runs only after the dispatching claim is
		// acquired, so the retry budget tracks actual re-dispatch attempts —
		// a tick that loses the claim to a concurrent goroutine (already
		// dispatching/advancing) never burns budget for a retry that didn't
		// happen.
		if e.handleTransientFetchRetry(t, step) {
			e.mu.Lock()
			delete(e.dispatching, t.ID)
			e.mu.Unlock()
			continue
		}

		// Re-read to guard against stale snapshots from concurrent ResumeStalled
		// calls: by the time we acquire dispatching, a prior goroutine may have
		// already advanced the workflow past this step.
		fresh, skip := e.shouldSkipResumeAfterFreshRead(t.ID, t.Workflow)
		if skip {
			e.mu.Lock()
			delete(e.dispatching, t.ID)
			e.mu.Unlock()
			continue
		}

		e.logger.Info("workflow.resume-stalled", "task_id", t.ID, "step", step.ID)
		comp, rErr := e.executeSteps(t.ID, &def, step, t.Workflow)
		e.mu.Lock()
		delete(e.dispatching, t.ID)
		e.mu.Unlock()
		// ResumeStalled only resumes async run_agent steps, so comp is normally
		// nil (fireComplete no-ops). Kept defensive + after the dispatching
		// marker is cleared, so the day a sync step becomes resumable its
		// completion cascades correctly instead of being silently dropped.
		e.fireComplete(comp)
		e.drainPendingConflictRecovery(t.ID)
		e.resumeError.Log(e.logger, "workflow.resume-stalled.exec", t.ID, rErr, "task_id", t.ID)
		if rErr != nil {
			e.surfaceStartFailure(t.ID, fresh.Status, rErr, fresh.Workflow, step.ID)
		} else {
			e.clearTransientFetchRetry(fresh.ID, fresh.Workflow, step.ID)
			e.clearCircuitBreakerOnSuccess(fresh.ID, fresh.Workflow, step.ID)
		}
	}
}

func (e *Engine) handleWatchdogHangRetry(t *TaskInfo, step *Step) bool {
	if t == nil || t.Workflow == nil || !isWatchdogHangReason(t.StatusReason) {
		return false
	}
	if step.Type != StepRunAgent {
		return false
	}
	// A tracked agent for this task+step may still be mid-completion-routing
	// even though HasRunningAgent already returned false (see the agentSteps
	// comment in ResumeStalled). Treating that window as a hang would burn
	// retry budget and clear the hang marker without a clean re-dispatch
	// actually happening.
	if e.hasTrackedAgentForTaskStep(t.ID, step.ID) {
		return false
	}
	retryKey := watchdogHangRetryKey(step.ID)
	attempts := parseWorkflowInt(t.Workflow.Variables[retryKey])
	if attempts >= maxWatchdogHangRetries {
		reason := fmt.Sprintf("watchdog hang: retry budget exhausted after %d clean re-dispatches", attempts)
		if err := e.tasks.UpdateTaskStatus(t.ID, "human-required", reason); err != nil {
			e.logger.Error("workflow.watchdog-hang.escalate", "task_id", t.ID, "step", step.ID, "err", err)
		} else {
			e.logger.Warn("workflow.watchdog-hang.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		}
		return true
	}
	t.Workflow.SetVar(retryKey, strconv.Itoa(attempts+1))
	cleanRef := t.Workflow.Variables[tamperBaselineVar(step.ID)]
	if cleanRef == "" {
		cleanRef = "HEAD"
	}
	t.Workflow.SetVar(watchdogHangCleanRetryKey(step.ID), cleanRef)
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		e.logger.Error("workflow.watchdog-hang.persist", "task_id", t.ID, "step", step.ID, "err", err)
		return true
	}
	if err := e.tasks.UpdateTaskStatus(t.ID, t.Status, ""); err != nil {
		e.logger.Error("workflow.watchdog-hang.clear", "task_id", t.ID, "step", step.ID, "err", err)
		return true
	}
	e.logger.Info("workflow.watchdog-hang.retry",
		"task_id", t.ID, "step", step.ID, "attempt", attempts+1, "max", maxWatchdogHangRetries)
	return false
}

func isWatchdogHangReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == watchdogHangStatusReasonPrefix || strings.HasPrefix(reason, watchdogHangStatusReasonPrefix+":")
}

func (e *Engine) handleWatchdogRateLimitRetry(t *TaskInfo, step *Step) bool {
	if t == nil || t.Workflow == nil || step == nil || step.Type != StepRunAgent || !isWatchdogRateLimitReason(t.StatusReason) {
		return false
	}
	retryKey := watchdogRateLimitRetryKey(step.ID)
	attempts := parseWorkflowInt(t.Workflow.Variables[retryKey])
	if attempts >= maxWatchdogRateLimitRetries {
		reason := fmt.Sprintf("watchdog: rate limit retry budget exhausted after %d clean re-dispatches", attempts)
		if err := e.tasks.UpdateTaskStatus(t.ID, "human-required", reason); err != nil {
			e.logger.Error("workflow.watchdog-rate-limit.escalate", "task_id", t.ID, "step", step.ID, "err", err)
		} else {
			e.logger.Warn("workflow.watchdog-rate-limit.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		}
		return true
	}
	t.Workflow.SetVar(retryKey, strconv.Itoa(attempts+1))
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		e.logger.Error("workflow.watchdog-rate-limit.persist", "task_id", t.ID, "step", step.ID, "err", err)
		return true
	}
	e.logger.Info("workflow.watchdog-rate-limit.retry",
		"task_id", t.ID, "step", step.ID, "attempt", attempts+1, "max", maxWatchdogRateLimitRetries)
	return false
}

func isWatchdogRateLimitReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	return reason == watchdogRateLimitStatusPrefix || strings.HasPrefix(reason, watchdogRateLimitStatusPrefix+":")
}

func (e *Engine) handleTransientFetchRetry(t *TaskInfo, step *Step) bool {
	if t == nil || t.Workflow == nil || step == nil || step.Type != StepRunAgent || !isTransientFetchReason(t.StatusReason) {
		return false
	}
	retryKey := transientFetchRetryKey(step.ID)
	attempts := parseWorkflowInt(t.Workflow.Variables[retryKey])
	if attempts >= maxTransientFetchRetries {
		reason := fmt.Sprintf("agent start blocked: transient network retry budget exhausted after %d attempts reconciling worktree with remote", attempts)
		if err := e.tasks.UpdateTaskStatus(t.ID, "human-required", reason); err != nil {
			e.logger.Error("workflow.transient-fetch.escalate", "task_id", t.ID, "step", step.ID, "err", err)
		} else {
			e.logger.Warn("workflow.transient-fetch.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		}
		return true
	}
	t.Workflow.SetVar(retryKey, strconv.Itoa(attempts+1))
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		e.logger.Error("workflow.transient-fetch.persist", "task_id", t.ID, "step", step.ID, "err", err)
		return true
	}
	e.logger.Info("workflow.transient-fetch.retry",
		"task_id", t.ID, "step", step.ID, "attempt", attempts+1, "max", maxTransientFetchRetries)
	return false
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

func watchdogHangRetryKey(stepID string) string {
	return watchdogHangRetryVarPrefix + stepID
}

func watchdogHangCleanRetryKey(stepID string) string {
	return watchdogHangCleanRetryVarPrefix + stepID
}

func watchdogRateLimitRetryKey(stepID string) string {
	return watchdogRateLimitRetryVarPrefix + stepID
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
	// A pre-agent-start rebase failure is the same "task branch conflicts
	// with base" condition push_branch/create_pr hit further down the
	// pipeline (see pushTaskBranch's project.ErrDivergedNeedsResolve branch) —
	// try the same autonomous branch-conflict-fix recovery before parking the
	// task on a human. currentStatus != "human-required" mirrors the sticky
	// guard below: don't re-trigger recovery for a task a concurrent handler
	// already parked.
	if currentStatus != "human-required" && e.conflictRecovery != nil && errors.Is(err, worktreeerr.ErrRebaseFailed) {
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
	reason, permanent := ClassifyAgentStartError(err)
	if reason == "" {
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
	if permanent {
		target = "human-required"
	}
	// A provider being rate-limited right now is a transient capacity condition,
	// not a genuine start failure: counting it toward the breaker would trip a
	// task to human-required for something that self-heals when the cooldown
	// window expires. Only genuine failures (bad worktree, missing project,
	// crashes) — and auth failures that need a human login — feed the breaker.
	if wf != nil && stepID != "" && !isTransientCapacityError(err) {
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
			reason = fmt.Sprintf("circuit breaker: %s (tripped after %d dispatch failures for step %q within %s)",
				reason, attempts, stepID, circuitBreakerWindow)
			e.logger.Warn("workflow.circuit-breaker.tripped",
				"task_id", taskID, "step", stepID, "attempts", attempts)
		}
		if setErr := e.tasks.SetWorkflow(taskID, wf); setErr != nil {
			e.logger.Error("workflow.circuit-breaker.persist", "task_id", taskID, "step", stepID, "err", setErr)
		}
	}
	if uErr := e.tasks.UpdateTaskStatus(taskID, target, reason); uErr != nil {
		e.logger.Error("workflow.resume-stalled.surface", "task_id", taskID, "err", uErr)
	}
}

func circuitBreakerFailureKey(stepID string) string { return circuitBreakerFailureVarPrefix + stepID }
func circuitBreakerFirstKey(stepID string) string   { return circuitBreakerFirstVarPrefix + stepID }

// recordCircuitBreakerFailure is the generic counterpart to
// handleWatchdogHangRetry's retry budget: instead of bounding retries of one
// specific known failure signature, it bounds ANY repeated agent-start
// failure for the same (task, step), regardless of cause. Once the count
// crosses maxCircuitBreakerFailures within circuitBreakerWindow, the caller
// trips the breaker.
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
