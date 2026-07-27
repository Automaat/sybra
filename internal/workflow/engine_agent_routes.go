package workflow

import (
	"slices"
	"strings"
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
	defer e.clearPendingAgentStep(taskID, agentID)
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

func pendingAgentRouteKey(taskID, agentID string) string {
	return taskID + "\x00" + agentID
}

func (e *Engine) setPendingAgentStep(taskID, agentID, stepID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setPendingAgentStepLocked(taskID, agentID, stepID)
}

func (e *Engine) setPendingAgentStepLocked(taskID, agentID, stepID string) {
	if taskID == "" || agentID == "" || stepID == "" {
		return
	}
	if e.pendingRoutes == nil {
		e.pendingRoutes = make(map[string]string)
	}
	e.pendingRoutes[pendingAgentRouteKey(taskID, agentID)] = stepID
}

func (e *Engine) clearPendingAgentStep(taskID, agentID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clearPendingAgentStepLocked(taskID, agentID)
}

func (e *Engine) clearPendingAgentStepLocked(taskID, agentID string) {
	if e.pendingRoutes == nil || agentID == "" {
		return
	}
	if taskID != "" {
		delete(e.pendingRoutes, pendingAgentRouteKey(taskID, agentID))
		return
	}
	suffix := "\x00" + agentID
	for key := range e.pendingRoutes {
		if strings.HasSuffix(key, suffix) {
			delete(e.pendingRoutes, key)
		}
	}
}

func (e *Engine) clearPendingAgentStepsForTask(taskID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pendingRoutes == nil || taskID == "" {
		return
	}
	prefix := taskID + "\x00"
	for key := range e.pendingRoutes {
		if strings.HasPrefix(key, prefix) {
			delete(e.pendingRoutes, key)
		}
	}
}

func (e *Engine) deferStartedAgentRoute(taskID, stepID, agentID string, err error) error {
	e.setPendingAgentStep(taskID, agentID, stepID)
	e.logger.Warn("workflow.agent-route.defer",
		"task_id", taskID, "step", stepID, "agent_id", agentID, "err", err)
	return errWorkflowYield
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
	e.clearPendingAgentStepsForTask(taskID)
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
