package workflow

import "strconv"

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
func (e *Engine) findReachableWaitHumanByStatus(def *Definition, current *Step, t TaskInfo, status string) (*Step, bool, error) {
	if current == nil {
		return nil, false, nil
	}
	if current.Type == StepWaitHuman {
		if current.Config.Status == status {
			return current, true, nil
		}
		return nil, false, nil
	}
	fields := e.transitionFields(t, t.Workflow)
	nextID, err := ResolveTransition(current.Next, fields)
	if err != nil || nextID == "" {
		return nil, false, err
	}
	for range maxSyncSteps {
		step := def.StepByID(nextID)
		if step == nil {
			return nil, false, nil
		}
		switch step.Type {
		case StepWaitHuman:
			if step.Config.Status == status {
				return step, true, nil
			}
			return nil, false, nil
		case StepRunAgent, StepParallel, StepBestOfN:
			return nil, false, nil
		case StepSetStatus:
			if step.Config.Status != "" {
				fields["task.status"] = step.Config.Status
			}
		case StepCondition, StepShell, StepEnsurePRClosesIssue, StepStampPRAttribution,
			StepRerequestReview, StepVerifyCommits, StepLinkPRAndReview, StepEvaluate,
			StepRequireSidecar, StepClearPlanArtifacts, StepValidatePlan,
			StepValidatePlanContract, StepTriageReview, StepFlagPlanCritique,
			StepDetectTampering, StepVerifyChecks, StepFocusedChecks,
			StepRoutePRFixResult, StepRouteTestResult, StepSyncBranch,
			StepCodegenGate, StepResumeWorkflow, StepPromoteBestOfN, StepPushBranch,
			StepCreatePR, StepClassifyTask:
			// Purely synchronous step: keep walking the declared transitions.
		}
		nextID, err = ResolveTransition(step.Next, fields)
		if err != nil || nextID == "" {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func (e *Engine) reconcileCurrentStepFromStatus(taskID string, t TaskInfo, def *Definition, status string) (TaskInfo, bool, error) {
	if t.Workflow == nil || t.Workflow.CurrentStep == "" || status == "" {
		return t, false, nil
	}
	current := def.StepByID(t.Workflow.CurrentStep)
	target, found, err := e.findReachableWaitHumanByStatus(def, current, t, status)
	if err != nil || !found || target.ID == t.Workflow.CurrentStep {
		return t, false, err
	}

	wf := t.Workflow.Clone()
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
