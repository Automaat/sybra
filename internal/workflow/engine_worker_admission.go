package workflow

import (
	"strconv"
	"time"
)

func admissionIntentID(base string, generation uint64) string {
	if generation == 0 {
		return base // retain identities of already-persisted dispatches
	}
	return base + ":admission:" + strconv.FormatUint(generation, 10)
}

// A remote worker may lose readiness between reservation and Start delivery.
// The provider never ran, so retire only that dispatch, retain the same step,
// and let the normal admission/ResumeStalled path wait for an eligible worker.
// In particular do not AdvanceStep: that consumes the coding retry budget and
// may salvage an unrelated stale sidecar as if execution had succeeded.
func (e *Engine) deferWorkerAdmission(taskID, agentID string) {
	unlock := e.acquireInflight(taskID)
	defer unlock()
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return
	}
	if _, skip := resumeSkipReasonForStatus(t.Status); skip || t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		e.clearAgentStep(taskID, agentID)
		return
	}
	def, err := e.resolveExecutionDefinition(taskID, t)
	if err != nil {
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		return
	}
	wf := t.Workflow.Clone()
	// Recheck exact ownership after taking the task lock. A generic pending
	// effect is not proof that this particular agent owns it (duplicate events
	// can arrive while the replacement effect is pending).
	e.mu.Lock()
	routedStep, status := e.resolveCompletionRouteLocked(t, AgentCompletion{AgentID: agentID})
	e.mu.Unlock()
	if status != taskStepTracked || !routeMatchesStep(step, routedStep) {
		return
	}
	// Only the rejected slot gets a new identity; siblings retain both their
	// live routes and their accepted/completed dispatch identities.
	if step.Type == StepParallel {
		parent := wf.ParallelInflight[step.ID]
		if parent == nil || parent.Children[routedStep] == nil {
			return
		}
		child := parent.Children[routedStep]
		if child.Status != "pending" || (child.AgentID != "" && child.AgentID != agentID) {
			return
		}
		child.AdmissionGeneration++
		child.AgentID = ""
	}
	if step.Type == StepBestOfN {
		parentID, attemptID, ok := splitBestOfNAttemptStepKey(routedStep)
		parent := wf.BestOfNInflight[parentID]
		if !ok || parent == nil || parent.Attempts[attemptID] == nil {
			return
		}
		attempt := parent.Attempts[attemptID]
		if attempt.Status != "pending" || (attempt.AgentID != "" && attempt.AgentID != agentID) {
			return
		}
		attempt.AdmissionGeneration++
		attempt.AgentID = ""
	}
	clearAgentRouteFromWorkflow(wf, agentID)
	// Refusal is a completed *admission*, even if the dispatch callback raced
	// ahead of effect completion. Never replay that terminal remote run.
	wf.RecordEffectIntent(EffectID{
		Generation: t.Generation,
		StepSeq:    nextEffectStepSeq(wf, t.Generation, executionStepSeq(wf)),
		StepID:     step.ID, Pos: effectPosStepAction,
	}, e.now())
	wf.State = ExecWaiting
	wf.SetVar(workflowRetryAfterVar, e.now().Add(30*time.Second).Format(time.RFC3339))
	reason := ClassifyAgentStartFailure(ErrResourcePressure).Reason + " (remote worker admission)"
	if err := e.tasks.SetStatusAndWorkflow(taskID, string(t.Status), reason, wf); err != nil {
		e.logger.Error("workflow.worker-admission.persist", "task_id", taskID, "err", err)
		return
	}
	e.clearPendingAgentStep(taskID, agentID)
	e.logger.Info("workflow.worker-admission.deferred", "task_id", taskID, "step", step.ID)
}
