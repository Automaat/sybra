package workflow

import (
	"fmt"
	"slices"
)

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
