package workflow

import (
	"slices"
	"strings"
	"sync"
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

// enterCompletion marks taskID as having a completion in flight and returns the
// matching release. It brackets the window HandleAgentComplete opens between
// "the agent has finished" and "the engine has routed that completion", which
// is exactly the window a persisted AgentRoute used to stand in for. Callers
// outside the agent manager (recovery's lost-callback bridge) drive completions
// for agents the manager no longer reports as running, so liveness alone cannot
// describe that window — without this marker, pruneStaleAgentRoutes would read
// the route as orphaned and let ResumeStalled dispatch a duplicate.
//
// Counted rather than a set: a parallel step can have several children
// completing at once, and the last one out must be the one that clears the mark.
func (e *Engine) enterCompletion(taskID string) func() {
	if taskID == "" {
		return func() {}
	}
	e.mu.Lock()
	if e.completing == nil {
		e.completing = make(map[string]int)
	}
	e.completing[taskID]++
	e.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if n := e.completing[taskID]; n > 1 {
				e.completing[taskID] = n - 1
			} else {
				delete(e.completing, taskID)
			}
		})
	}
}

func (e *Engine) completionInFlight(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completing[taskID] > 0
}

// routeMatchesStep reports whether a persisted AgentRoute target belongs to
// step — directly, as a parallel child, or as a best_of_n attempt.
func routeMatchesStep(step *Step, routedStepID string) bool {
	if step == nil {
		return false
	}
	return routedStepID == step.ID || parallelHasChild(step, routedStepID) || bestOfNStepMatches(step, routedStepID)
}

// pruneStaleAgentRoutes drops persisted AgentRoutes for step whose agent no
// longer exists in any form: not running, not dispatching, and with no
// completion in flight.
//
// Such a route is unrecoverable on its own. Routes are only ever cleared by an
// agent-completion path, so a route that outlived its agent has nothing left to
// clear it, and tryMarkResumeDispatching reads it as "agent-pending-completion"
// on every tick — the task then stalls permanently with no error, no status
// change, and no escalation (#2824). Clearing it here is what makes the wedge
// recoverable; the WARN is what makes it visible.
//
// Liveness is task-scoped rather than agent-scoped on purpose: any running
// agent for the task means the route may still be legitimately owned, so this
// declines to prune. That errs toward the old (skip) behaviour, never toward
// duplicate dispatch.
//
// IsDispatching is consulted separately from HasRunningAgent rather than
// assumed to be covered by it: the AgentLauncher contract does not promise
// that a held dispatch claim reads as "running", and tryMarkResumeDispatching
// already treats the two as independent signals.
func (e *Engine) pruneStaleAgentRoutes(taskID string, step *Step) {
	if taskID == "" || step == nil {
		return
	}
	if e.completionInFlight(taskID) || e.resumeDispatching(taskID) || e.agents.HasRunningAgent(taskID) || e.agents.IsDispatching(taskID) {
		return
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil || t.Workflow == nil || len(t.Workflow.AgentRoutes) == 0 {
		return
	}
	stale := make([]string, 0, len(t.Workflow.AgentRoutes))
	for agentID, routedStepID := range t.Workflow.AgentRoutes {
		if routeMatchesStep(step, routedStepID) {
			stale = append(stale, agentID)
		}
	}
	if len(stale) == 0 {
		return
	}
	// Map iteration order is random; sort so the emitted WARNs are stable.
	slices.Sort(stale)
	for _, agentID := range stale {
		e.logger.Warn("workflow.resume-stalled.stale-route",
			"task_id", taskID, "step", step.ID, "agent_id", agentID,
			"route_step", t.Workflow.AgentRoutes[agentID])
		clearAgentRouteFromWorkflow(t.Workflow, agentID)
	}
	if err := e.tasks.SetWorkflow(taskID, t.Workflow); err != nil {
		e.logger.Warn("workflow.resume-stalled.stale-route.clear",
			"task_id", taskID, "step", step.ID, "err", err)
		return
	}
	e.mu.Lock()
	for _, agentID := range stale {
		e.clearPendingAgentStepLocked(taskID, agentID)
	}
	e.mu.Unlock()
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

func (e *Engine) hasPendingAgentRouteForStep(taskID string, step *Step) bool {
	if step == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, routedStepID := range e.pendingRoutes {
		if strings.HasPrefix(key, taskID+"\x00") && routeMatchesStep(step, routedStepID) {
			return true
		}
	}
	return false
}

func workflowHasAgentRouteForStep(wf *Execution, step *Step) bool {
	if wf == nil || step == nil {
		return false
	}
	for _, routedStepID := range wf.AgentRoutes {
		if routeMatchesStep(step, routedStepID) {
			return true
		}
	}
	return false
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
