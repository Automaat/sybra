package workflow

import "time"

// routeStepPending reports whether the task's current step is still inside its
// claimed step-action effect window: intent persisted, completion not yet
// recorded. This replaces the old in-memory "start in flight" marker.
func routeStepPending(t TaskInfo, stepID string) bool {
	if t.Workflow == nil || stepID == "" || t.Workflow.CurrentStep != stepID {
		return false
	}
	currentSeq := executionStepSeq(t.Workflow)
	for i := len(t.Workflow.EffectLog) - 1; i >= 0; i-- {
		rec := t.Workflow.EffectLog[i]
		if rec.ID.StepID == stepID && rec.ID.Pos == effectPosStepAction && rec.ID.StepSeq >= currentSeq && rec.CompletedAt == nil {
			return true
		}
	}
	return false
}

func parallelChildStepByAgentID(wf *Execution, parentStepID, agentID string) (string, bool) {
	if wf == nil || parentStepID == "" || agentID == "" {
		return "", false
	}
	parent := wf.ParallelInflight[parentStepID]
	if parent == nil {
		return "", false
	}
	for childID, child := range parent.Children {
		if child != nil && child.AgentID == agentID {
			return childID, true
		}
	}
	return "", false
}

func bestOfNAttemptStepByAgentID(wf *Execution, parentStepID, agentID string) (string, bool) {
	if wf == nil || parentStepID == "" || agentID == "" {
		return "", false
	}
	parent := wf.BestOfNInflight[parentStepID]
	if parent == nil {
		return "", false
	}
	for attemptID, attempt := range parent.Attempts {
		if attempt != nil && attempt.AgentID == agentID {
			return bestOfNAttemptStepKey(parentStepID, attemptID), true
		}
	}
	return "", false
}

func (e *Engine) lookupAgentStep(taskID, agentID string) (string, bool) {
	if taskID == "" || agentID == "" {
		return "", false
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return "", false
	}
	return t.Workflow.AgentRoute(agentID)
}

func (e *Engine) clearAgentStep(taskID, agentID string) {
	if agentID == "" {
		return
	}
	if taskID == "" {
		tasks, err := e.tasks.ListTasks()
		if err != nil {
			return
		}
		for i := range tasks {
			if tasks[i].Workflow == nil || !clearAgentRouteFromWorkflow(tasks[i].Workflow, agentID) {
				continue
			}
			_ = e.tasks.SetWorkflow(tasks[i].ID, tasks[i].Workflow)
		}
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return
	}
	if !clearAgentRouteFromWorkflow(t.Workflow, agentID) {
		return
	}
	_ = e.tasks.SetWorkflow(taskID, t.Workflow)
}

func clearAgentRouteFromWorkflow(wf *Execution, agentID string) bool {
	if wf == nil || agentID == "" {
		return false
	}
	_, hadRoute := wf.AgentRoute(agentID)
	wf.ClearAgentRoute(agentID)
	for _, parent := range wf.ParallelInflight {
		if parent == nil {
			continue
		}
		for _, child := range parent.Children {
			if child != nil && child.AgentID == agentID && child.Status == "pending" {
				child.AgentID = ""
			}
		}
	}
	for _, parent := range wf.BestOfNInflight {
		if parent == nil {
			continue
		}
		for _, attempt := range parent.Attempts {
			if attempt != nil && attempt.AgentID == agentID && attempt.Status == "pending" {
				attempt.AgentID = ""
			}
		}
	}
	return hadRoute
}

func (e *Engine) clearAgentStepsForTask(taskID string) {
	if taskID == "" {
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return
	}
	t.Workflow.ClearAgentRoutes()
	_ = e.tasks.SetWorkflow(taskID, t.Workflow)
}

// resolveCompletionRoute is kept as a thin compatibility wrapper for tests.
func (e *Engine) resolveCompletionRoute(taskID, _ string, c AgentCompletion) (string, taskStepStatus) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return "", taskStepFree
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resolveCompletionRouteLocked(t, c)
}

// markStepStarting / unmarkStepStartingAndTakePending are test-only
// compatibility shims for the deleted pending-start machinery. They now model
// the same state through the current step's pending action effect instead of an
// Engine-local buffer.
func (e *Engine) markStepStarting(taskID, stepID string) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return
	}
	stepSeq := nextEffectStepSeq(t.Workflow, t.Generation, executionStepSeq(t.Workflow))
	t.Workflow.RecordEffectIntent(EffectID{
		Generation: t.Generation,
		StepSeq:    stepSeq,
		StepID:     stepID,
		Pos:        effectPosStepAction,
	}, time.Now().UTC())
	_ = e.tasks.SetWorkflow(taskID, t.Workflow)
}

func (e *Engine) unmarkStepStartingAndTakePending(taskID, stepID string) []AgentCompletion {
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return nil
	}
	for i := len(t.Workflow.EffectLog) - 1; i >= 0; i-- {
		rec := t.Workflow.EffectLog[i]
		if rec.ID.StepID != stepID || rec.ID.Pos != effectPosStepAction || rec.CompletedAt != nil {
			continue
		}
		t.Workflow.RecordEffectCompletion(rec.ID, time.Now().UTC())
		break
	}
	_ = e.tasks.SetWorkflow(taskID, t.Workflow)
	return nil
}

func (e *Engine) clearPendingStepEffect(taskID string, id EffectID) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || id.IsZero() {
		return
	}
	kept := t.Workflow.EffectLog[:0]
	changed := false
	for i := range t.Workflow.EffectLog {
		rec := t.Workflow.EffectLog[i]
		if rec.ID.Equal(id) && rec.CompletedAt == nil {
			changed = true
			continue
		}
		kept = append(kept, rec)
	}
	if !changed {
		return
	}
	t.Workflow.EffectLog = kept
	_ = e.tasks.SetWorkflow(taskID, t.Workflow)
}
