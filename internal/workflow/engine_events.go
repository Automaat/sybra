package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
)

// HandleHumanAction processes approve/reject/input from the UI.
func (e *Engine) HandleHumanAction(taskID, action string, data map[string]string) error {
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
// Silently skips (Debug log) when the task's workflow is already terminal or
// has no current step. Agents that were started outside the workflow engine
// (e.g. manual pr-fix retries, recovery spawns) land here on completion; the
// guard avoids the "step not found" error loop that followed workflow
// completion in older versions.
func (e *Engine) HandleAgentComplete(taskID string, c AgentCompletion) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Error("workflow.agent-complete.get", "task_id", taskID, "err", err)
		return
	}
	if t.Workflow == nil {
		e.logger.Debug("workflow.agent-complete.no-workflow", "task_id", taskID)
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
		e.logger.Debug("workflow.agent-complete.terminal",
			"task_id", taskID, "agent_id", c.AgentID, "state", string(t.Workflow.State))
		e.clearAgentStep(c.AgentID)
		return
	}
	if t.Workflow.CurrentStep == "" {
		e.logger.Debug("workflow.agent-complete.no-current-step",
			"task_id", taskID, "agent_id", c.AgentID, "state", string(t.Workflow.State))
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
			e.logger.Debug("workflow.agent-complete.untracked-ignored",
				"task_id", taskID, "agent_id", c.AgentID, "current_step", spawnedStep)
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
		if t.Status != "" {
			e.surfaceStartFailure(taskID, t.Status, err)
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
// flight for the given task+step pair. Used to detect phantom completions from
// untracked (manually-dispatched) agents.
func (e *Engine) hasTrackedAgentForTaskStep(taskID, stepID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, entry := range e.agentSteps {
		if entry.taskID == taskID && entry.stepID == stepID {
			return true
		}
	}
	return false
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
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || t.Workflow.CurrentStep == "" {
		e.clearAgentStep(agentID)
		return
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed || t.Status == "human-required" {
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

	e.logger.Info("workflow.rate-limit-reschedule", "task_id", taskID, "step", step.ID)
	comp, rErr := e.executeSteps(taskID, &def, step, t.Workflow)
	e.mu.Lock()
	delete(e.dispatching, taskID)
	e.mu.Unlock()
	e.fireComplete(comp)
	e.resumeError.Log(e.logger, "workflow.rate-limit-reschedule.exec", taskID, rErr, "task_id", taskID)
	if rErr != nil {
		e.surfaceStartFailure(taskID, t.Status, rErr)
	}
}

func (e *Engine) rescheduleRateLimitedParallelChild(taskID, agentID string, parent, child *Step, t TaskInfo) {
	e.acquireInflight(taskID)
	defer e.releaseInflight(taskID)

	fresh, err := e.tasks.GetTask(taskID)
	if err != nil || fresh.Workflow == nil || fresh.Workflow.CurrentStep != parent.ID || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed || fresh.Status == "human-required" {
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
	}
	if setErr := e.tasks.SetWorkflow(taskID, wfExec); setErr != nil {
		e.logger.Error("workflow.rate-limit-reschedule.parallel.set", "task_id", taskID, "parent", parent.ID, "child", child.ID, "err", setErr)
	}
	if spawnErr != nil {
		e.surfaceStartFailure(taskID, fresh.Status, spawnErr)
	}
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

		// Only resume async agent steps where no agent is running.
		if step.Type != StepRunAgent && step.Type != StepParallel {
			continue
		}
		if retryAt, ok := workflowRetryAfter(t.Workflow); ok && time.Now().Before(retryAt) {
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", "retry_after", "retry_after", retryAt.Format(time.RFC3339), "step", step.ID)
			continue
		}
		// A task in human-required was halted by a competing path (e.g. the
		// inline review triage deciding the PR is too small). Do not resume its
		// workflow: that would override the triage verdict and re-dispatch an
		// agent that the operator already suppressed.
		if t.Status == "human-required" {
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", "human_required", "step", step.ID)
			continue
		}
		if e.agents.HasRunningAgent(t.ID) {
			continue
		}
		if e.shouldSkipResumeForRateLimitedProvider(t, step) {
			continue
		}
		// Skip tasks whose step is currently being dispatched. Interactive
		// spawns (worktree creation, rebase, agent process start) take
		// several seconds during which no agent is yet registered — without
		// this guard the ticker would spawn a duplicate and the second
		// agent's completion would corrupt the workflow at the wait_human
		// gate.
		// inflightMutexes is a non-blocking probe: TryLock distinguishes
		// "another goroutine currently holds the advance lock" from "free".
		// We only set dispatching when both the advance lock and prior
		// dispatching guard are free.
		mu := e.taskInflightMutex(t.ID)
		advancing := !mu.TryLock()
		if !advancing {
			mu.Unlock()
		}
		e.mu.Lock()
		_, dispatching := e.dispatching[t.ID]
		// agentSteps holds outstanding agents the engine spawned but hasn't
		// yet routed completion for. Required because interactive agents pass
		// through StatePaused after their first result event (one-shot path
		// closes stdin → state Paused → process exits → onComplete fires →
		// AdvanceStep), and HasRunningAgent returns false during that window.
		// Without this check a tight ResumeStalled loop dispatches a duplicate.
		hasOutstandingAgent := false
		for _, entry := range e.agentSteps {
			if entry.taskID == t.ID && (entry.stepID == step.ID || parallelHasChild(step, entry.stepID)) {
				hasOutstandingAgent = true
				break
			}
		}

		if !advancing && !dispatching && !hasOutstandingAgent {
			e.dispatching[t.ID] = struct{}{}
		}
		e.mu.Unlock()
		if advancing || dispatching || hasOutstandingAgent {
			reason := "dispatching"
			switch {
			case advancing:
				reason = "inflight"
			case hasOutstandingAgent:
				reason = "agent-pending-completion"
			}
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", reason, "step", step.ID)
			continue
		}

		// Re-read to guard against stale snapshots from concurrent ResumeStalled
		// calls: by the time we acquire dispatching, a prior goroutine may have
		// already advanced the workflow past this step.
		fresh, fErr := e.tasks.GetTask(t.ID)
		if fErr != nil || fresh.Workflow == nil || fresh.Workflow.CurrentStep != t.Workflow.CurrentStep || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed || fresh.Status == "human-required" {
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
		e.resumeError.Log(e.logger, "workflow.resume-stalled.exec", t.ID, rErr, "task_id", t.ID)
		if rErr != nil {
			e.surfaceStartFailure(t.ID, fresh.Status, rErr)
		}
	}
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
func (e *Engine) surfaceStartFailure(taskID, currentStatus string, err error) {
	reason, permanent := ClassifyAgentStartError(err)
	if reason == "" {
		return
	}
	target := currentStatus
	if permanent {
		target = "human-required"
	}
	if uErr := e.tasks.UpdateTaskStatus(taskID, target, reason); uErr != nil {
		e.logger.Error("workflow.resume-stalled.surface", "task_id", taskID, "err", uErr)
	}
}
