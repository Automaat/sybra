package workflow

import (
	"slices"
	"strings"
	"time"
)

// routeStepPending reports whether the task's current step is still inside its
// claimed step-action effect window: intent persisted, completion not yet
// recorded. This replaces the old in-memory "start in flight" marker.
func routeStepPending(t TaskInfo, stepID string) bool {
	if t.Workflow == nil || stepID == "" || t.Workflow.CurrentStep != stepID {
		return false
	}
	currentSeq := executionStepSeq(t.Workflow)
	for _, rec := range slices.Backward(t.Workflow.EffectLog) {
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
	e.clearBufferedCompletionsForTask(taskID)
}

// markStepStarting preserves test compatibility for the deleted pending-start
// machinery. unmarkStepStartingAndTakePending drains completions that arrived
// before execRunAgent could persist the returned agent route.
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
	e.mu.Lock()
	defer e.mu.Unlock()

	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil {
		return nil
	}
	for _, rec := range slices.Backward(t.Workflow.EffectLog) {
		if rec.ID.StepID != stepID || rec.ID.Pos != effectPosStepAction || rec.CompletedAt != nil {
			continue
		}
		t.Workflow.RecordEffectCompletion(rec.ID, time.Now().UTC())
		break
	}
	_ = e.tasks.SetWorkflow(taskID, t.Workflow)
	return e.takeBufferedCompletionsLocked(taskID, stepID)
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
	e.clearBufferedCompletions(taskID, id.StepID)
}

func pendingCompletionKey(taskID, stepID string) string {
	return taskID + "\x00" + stepID
}

func (e *Engine) bufferCompletionLocked(taskID, stepID string, c AgentCompletion) {
	if taskID == "" || stepID == "" {
		return
	}
	if e.pendingComplete == nil {
		e.pendingComplete = make(map[string][]AgentCompletion)
	}
	key := pendingCompletionKey(taskID, stepID)
	e.pendingComplete[key] = append(e.pendingComplete[key], c)
}

func (e *Engine) takeBufferedCompletionsLocked(taskID, stepID string) []AgentCompletion {
	if e.pendingComplete == nil {
		return nil
	}
	key := pendingCompletionKey(taskID, stepID)
	pending := e.pendingComplete[key]
	delete(e.pendingComplete, key)
	return slices.Clone(pending)
}

func (e *Engine) clearBufferedCompletions(taskID, stepID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.takeBufferedCompletionsLocked(taskID, stepID)
}

func (e *Engine) clearBufferedCompletionsForTask(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pendingComplete == nil || taskID == "" {
		return
	}
	prefix := taskID + "\x00"
	for key := range e.pendingComplete {
		if strings.HasPrefix(key, prefix) {
			delete(e.pendingComplete, key)
		}
	}
}
