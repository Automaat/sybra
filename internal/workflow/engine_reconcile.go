package workflow

import (
	"errors"
	"strconv"
)

// asyncBoundaryComplete reports whether a StepParallel/StepBestOfN step has
// no pending children/attempts left, i.e. it is safe for status-driven
// reconciliation to treat it as passable. A missing inflight record reports
// incomplete (blocking), not complete: resolveNext persists CurrentStep
// pointing at the parallel/best-of-n step *before* execParallel/execBestOfN
// creates and persists its inflight record (a separate write, with no lock
// held across it — reconcileCurrentStepFromStatus runs on the status-change
// caller's own goroutine, unsynchronized with executeSteps), so CurrentStep
// can legitimately equal this step's ID while the record doesn't exist yet.
// Treating that pre-spawn window as "complete" would let reconciliation skip
// the boundary entirely, and the children's eventual completion callbacks
// would then find no record to record against and drop silently. Blocking
// here instead costs at most the brief spawn window; a genuinely stuck task
// still falls to the existing dwell/run-rate watchdogs, not this check.
// Any other step type is never a boundary and always reports complete.
func (e *Engine) asyncBoundaryComplete(wf *Execution, step *Step) bool {
	if wf == nil || step == nil {
		return true
	}
	switch step.Type {
	case StepParallel:
		if rec := wf.ParallelInflight[step.ID]; rec != nil {
			return rec.AllChildrenDone()
		}
		return false
	case StepBestOfN:
		if rec := wf.BestOfNInflight[step.ID]; rec != nil {
			return rec.AllAttemptsDone()
		}
		return false
	default:
		return true
	}
}

// transitionFields mirrors resolveNext's field set for lightweight
// "where would the workflow go now?" previews used by stale-step repair.
func (e *Engine) transitionFields(t TaskInfo, wf *Execution) map[string]string {
	fields := taskFields(t)
	if wf != nil {
		for k, v := range wf.Variables {
			fields["vars."+k] = v
		}
		if wf.Recovered {
			fields["vars.recovered"] = "true"
		}
	}
	fields["config.review_until_clean"] = strconv.FormatBool(!e.reviewLoopDisabled)
	return fields
}

// findReachableWaitHumanByStatus walks the current workflow path without
// executing steps, stopping at the first async boundary. Used when a human or
// external tool already moved task.Status to the downstream wait_human gate.
func (e *Engine) findReachableWaitHumanByStatus(def *Definition, current *Step, t TaskInfo, status string) (target *Step, found, mustExecute bool, err error) {
	if current == nil {
		return nil, false, false, nil
	}
	if current.Type == StepWaitHuman {
		if current.Config.Status == status {
			return current, true, false, nil
		}
		return nil, false, false, nil
	}
	switch current.Type {
	case StepRunAgent, StepCondition, StepSetStatus, StepRequireSidecar, StepFlagPlanCritique, StepParallel, StepBestOfN:
	default:
		return nil, false, false, nil
	}
	mustExecute = current.Type == StepRequireSidecar || current.Type == StepFlagPlanCritique
	fields := e.transitionFields(t, t.Workflow)
	if current.Type == StepSetStatus && current.Config.Status != "" {
		fields["task.status"] = current.Config.Status
		mustExecute = true
	}
	nextID, err := ResolveTransition(current.Next, fields)
	if err != nil || nextID == "" {
		return nil, false, false, err
	}
	for range maxSyncSteps {
		step := def.StepByID(nextID)
		if step == nil {
			return nil, false, false, nil
		}
		switch step.Type {
		case StepWaitHuman:
			if step.Config.Status == status {
				return step, true, mustExecute, nil
			}
			return nil, false, false, nil
		case StepRunAgent, StepParallel, StepBestOfN:
			return nil, false, false, nil
		case StepSetStatus:
			if step.Config.Status != "" {
				fields["task.status"] = step.Config.Status
			}
			mustExecute = true
		case StepRequireSidecar, StepFlagPlanCritique:
			mustExecute = true
		case StepCondition:
			// Purely synchronous and side-effect-free: keep walking transitions.
		case StepShell, StepEnsurePRClosesIssue, StepStampPRAttribution,
			StepRerequestReview, StepVerifyCommits, StepLinkPRAndReview, StepEvaluate,
			StepClearPlanArtifacts, StepValidatePlan,
			StepValidatePlanContract, StepTriageReview,
			StepDetectTampering, StepVerifyChecks, StepFocusedChecks,
			StepRoutePRFixResult, StepRouteTestResult, StepSyncBranch,
			StepCodegenGate, StepResumeWorkflow, StepPromoteBestOfN, StepPushBranch,
			StepCreatePR, StepClassifyTask, StepAdmissionPreflight, StepRequireEvidence:
			return nil, false, false, nil
		}
		nextID, err = ResolveTransition(step.Next, fields)
		if err != nil || nextID == "" {
			return nil, false, false, err
		}
	}
	return nil, false, false, nil
}

func (e *Engine) reconcileCurrentStepFromStatus(taskID string, t TaskInfo, def *Definition, status string) (TaskInfo, bool, error) {
	if t.Workflow == nil || t.Workflow.CurrentStep == "" || status == "" {
		return t, false, nil
	}
	current := def.StepByID(t.Workflow.CurrentStep)
	if fresh, reconciled, err := e.reconcileWaitHumanActionAlias(taskID, t, current, status); err != nil || reconciled {
		return fresh, reconciled, err
	}
	if current != nil && current.Type == StepRunAgent {
		if current.Config.WaitForStatus == status {
			return t, false, nil
		}
		if e.agents.HasRunningAgent(taskID) {
			e.logger.Debug("workflow.current-step.reconcile-status.skip",
				"task_id", taskID, "step", current.ID, "status", status, "reason", "running_agent")
			return t, false, nil
		}
	}
	if current != nil && !e.asyncBoundaryComplete(t.Workflow, current) {
		// current is StepParallel/StepBestOfN with children/attempts still
		// in flight: an external task.Status write matching some downstream
		// wait_human must not fast-forward CurrentStep past this boundary —
		// findReachableWaitHumanByStatus's own walk only stops at a *later*
		// StepParallel/StepBestOfN it encounters, never at the starting step
		// itself, so without this guard a status match could jump straight
		// to that wait_human while child/judge agents are still running.
		e.logger.Debug("workflow.current-step.reconcile-status.skip",
			"task_id", taskID, "step", current.ID, "status", status, "reason", "async_boundary_incomplete")
		return t, false, nil
	}
	target, found, mustExecute, err := e.findReachableWaitHumanByStatus(def, current, t, status)
	// found is only ever true alongside a non-nil target (every return in
	// findReachableWaitHumanByStatus pairs them), but the explicit nil check
	// lets nilaway verify that instead of trusting the bool correlation.
	if err != nil || !found || target == nil || target.ID == t.Workflow.CurrentStep {
		return t, false, err
	}
	if mustExecute {
		executed, err := e.executeCurrentStepForStatusReconcile(taskID, def, current, t, status)
		if err != nil {
			return TaskInfo{}, false, err
		}
		if !executed {
			return t, false, nil
		}
		fresh, err := e.tasks.GetTask(taskID)
		if err != nil {
			return TaskInfo{}, true, err
		}
		return fresh, true, nil
	}

	wf := t.Workflow.Clone()
	if wf == nil {
		// Unreachable: t.Workflow is non-nil (checked at function entry) and
		// Clone only returns nil for a nil receiver. Explicit check satisfies
		// nilaway, which can't see that invariant across the call.
		return TaskInfo{}, false, errors.New("reconcile: cloned workflow is nil")
	}
	wf.CurrentStep = target.ID
	wf.State = ExecWaiting
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		return TaskInfo{}, false, err
	}
	e.logger.Info("workflow.current-step.reconcile-status",
		"task_id", taskID, "from", t.Workflow.CurrentStep, "to", target.ID, "status", status)
	e.maybeAutoApprovePlanReview(taskID, target)

	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		return TaskInfo{}, true, err
	}
	return fresh, true, nil
}

func (e *Engine) reconcileWaitHumanActionAlias(taskID string, t TaskInfo, current *Step, status string) (TaskInfo, bool, error) {
	action, ok := waitHumanActionAlias(current, status)
	if !ok {
		return t, false, nil
	}
	if err := validateHumanAction(current, action); err != nil {
		return TaskInfo{}, false, err
	}
	wf := t.Workflow.Clone()
	if wf == nil {
		return TaskInfo{}, false, errors.New("reconcile: cloned workflow is nil")
	}
	wf.SetVar("human_action", action)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		return TaskInfo{}, false, err
	}
	if err := e.AdvanceStep(taskID, StepOutput{
		StepID: current.ID,
		Status: "completed",
		Output: action,
	}); err != nil {
		return TaskInfo{}, false, err
	}
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		return TaskInfo{}, true, err
	}
	e.logger.Info("workflow.current-step.reconcile-human-action",
		"task_id", taskID, "step", current.ID, "status", status, "action", action)
	return fresh, true, nil
}

func waitHumanActionAlias(step *Step, status string) (string, bool) {
	if step == nil || step.Type != StepWaitHuman || step.Config.Status != "plan-review" {
		return "", false
	}
	switch status {
	case "todo":
		return "approve", true
	case "planning":
		return "reject", true
	default:
		return "", false
	}
}

func (e *Engine) executeCurrentStepForStatusReconcile(taskID string, def *Definition, current *Step, t TaskInfo, status string) (bool, error) {
	if current == nil {
		// Unreachable in practice: mustExecute (this function's sole caller
		// gate) is only ever true alongside a non-nil current — see the
		// caller. Explicit check satisfies nilaway.
		return false, nil
	}
	e.logger.Info("workflow.current-step.reconcile-status.execute",
		"task_id", taskID, "from", t.Workflow.CurrentStep, "status", status)
	switch current.Type {
	case StepRunAgent, StepParallel, StepBestOfN:
		return true, e.AdvanceStep(taskID, StepOutput{
			StepID: current.ID,
			Status: "completed",
			Output: "status:" + status,
		})
	case StepCondition, StepSetStatus, StepRequireSidecar, StepFlagPlanCritique:
		wf := t.Workflow.Clone()
		if wf == nil {
			// Unreachable: t.Workflow is non-nil whenever this function's
			// caller reaches mustExecute — Clone only returns nil for a nil
			// receiver. Explicit check satisfies nilaway.
			return false, errors.New("reconcile: cloned workflow is nil")
		}
		return true, e.executeNextSteps(taskID, def, current, wf)
	default:
		return false, nil
	}
}

func (e *Engine) reconcileCurrentStepFromPriorCondition(taskID string, t TaskInfo, def *Definition) (TaskInfo, bool, error) {
	if t.Workflow == nil {
		return t, false, nil
	}
	last := t.Workflow.LastRecord()
	if last == nil {
		return t, false, nil
	}
	prior := def.StepByID(last.StepID)
	if prior == nil || prior.Type != StepCondition {
		return t, false, nil
	}

	nextID, err := ResolveTransition(prior.Next, e.transitionFields(t, t.Workflow))
	if err != nil || nextID == "" || nextID == t.Workflow.CurrentStep {
		return t, false, err
	}
	next := def.StepByID(nextID)
	if next == nil {
		return t, false, nil
	}
	if next.Type != StepWaitHuman && !isResumableStepType(next.Type) {
		return t, false, nil
	}

	wf := t.Workflow.Clone()
	if wf == nil {
		// Unreachable: t.Workflow is non-nil (checked at function entry) and
		// Clone only returns nil for a nil receiver. Explicit check satisfies
		// nilaway, which can't see that invariant across the call.
		return TaskInfo{}, false, errors.New("reconcile: cloned workflow is nil")
	}
	wf.CurrentStep = next.ID
	if next.Type == StepWaitHuman {
		wf.State = ExecWaiting
	} else {
		wf.State = ExecRunning
	}
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		return TaskInfo{}, false, err
	}
	if next.Type == StepWaitHuman && next.Config.Status != "" && t.Status != next.Config.Status {
		if err := e.tasks.UpdateTaskStatus(taskID, next.Config.Status, next.Config.StatusReason); err != nil {
			return TaskInfo{}, true, err
		}
		e.maybeAutoApprovePlanReview(taskID, next)
	}
	e.logger.Info("workflow.current-step.reconcile-condition",
		"task_id", taskID, "condition", prior.ID, "from", t.Workflow.CurrentStep, "to", next.ID)

	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		return TaskInfo{}, true, err
	}
	return fresh, true, nil
}
