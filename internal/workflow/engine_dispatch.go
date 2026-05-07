package workflow

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ErrWorkflowAlreadyActive is returned by DispatchEvent when the target task
// already has a non-terminal workflow attached.
var ErrWorkflowAlreadyActive = fmt.Errorf("task already has an active workflow")

// StartWorkflow assigns a workflow to a task and executes the first step.
func (e *Engine) StartWorkflow(taskID, workflowID string) error {
	return e.StartWorkflowWithVars(taskID, workflowID, nil)
}

// StartWorkflowWithVars assigns a workflow and seeds the execution with
// initial variables. Use the reserved WorkflowVarDir key to pass a
// pre-prepared working directory to run_agent steps.
//
// Serialized per task via the starting map so two concurrent callers
// (restart + UI button, two loop-agent ticks, etc) never both spawn agents
// for the same task. Second caller gets ErrWorkflowAlreadyActive.
func (e *Engine) StartWorkflowWithVars(taskID, workflowID string, vars map[string]string) error {
	e.mu.Lock()
	if _, busy := e.starting[taskID]; busy {
		e.mu.Unlock()
		return fmt.Errorf("%w: start in progress", ErrWorkflowAlreadyActive)
	}
	e.starting[taskID] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.starting, taskID)
		e.mu.Unlock()
	}()

	// Guard against sequential duplicate starts: the starting map only prevents
	// overlapping entries. If caller A has completed its Start* call (defer
	// removed the marker) while caller B is queued behind the mutex, B would
	// otherwise see an empty map and overwrite A's workflow. Mirror the check
	// DispatchEvent already performs so both entry points agree that a task
	// can only have one non-terminal workflow at a time.
	if t, getErr := e.tasks.GetTask(taskID); getErr == nil &&
		t.Workflow != nil &&
		t.Workflow.State != ExecCompleted &&
		t.Workflow.State != ExecFailed {
		return fmt.Errorf("%w: %s (state=%s)",
			ErrWorkflowAlreadyActive, t.Workflow.WorkflowID, t.Workflow.State)
	}

	def, err := e.store.Get(workflowID)
	if err != nil {
		return fmt.Errorf("get workflow %s: %w", workflowID, err)
	}

	first := def.FirstStep()
	if first == nil {
		return fmt.Errorf("workflow %s has no steps", workflowID)
	}

	variables := make(map[string]string, len(vars))
	maps.Copy(variables, vars)

	wfExec := &Execution{
		WorkflowID:  workflowID,
		CurrentStep: first.ID,
		State:       ExecRunning,
		Variables:   variables,
		StartedAt:   time.Now().UTC(),
	}

	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return fmt.Errorf("set workflow on task: %w", err)
	}

	e.logger.Info("workflow.start", "task_id", taskID, "workflow", workflowID, "step", first.ID)
	return e.executeSteps(taskID, &def, first, wfExec)
}

// MatchWorkflow finds the best workflow for a task based on trigger conditions.
func (e *Engine) MatchWorkflow(t TaskInfo, event string) *Definition {
	return e.matchWorkflow(t, event, nil)
}

// matchWorkflow evaluates trigger conditions against task fields plus extra
// event-specific fields (e.g. "pr.issue_kind" for pr.event dispatch) and
// returns the highest-priority matching definition. When multiple definitions
// share the same priority, the store's alphabetical order (by filename) is
// the deterministic tiebreaker.
func (e *Engine) matchWorkflow(t TaskInfo, event string, extra map[string]string) *Definition {
	defs, err := e.store.List()
	if err != nil {
		e.logger.Error("workflow.match.list", "err", err)
		return nil
	}

	fields := taskFields(t)
	maps.Copy(fields, extra)

	var matches []*Definition
	for i := range defs {
		if defs[i].Trigger.On != event {
			continue
		}
		if EvalConditions(defs[i].Trigger.Conditions, fields) {
			matches = append(matches, &defs[i])
		}
	}
	if len(matches) == 0 {
		return nil
	}
	// Stable sort preserves store order (alphabetical) within the same
	// priority bucket, so tiebreaks stay deterministic across runs.
	slices.SortStableFunc(matches, func(a, b *Definition) int {
		return cmp.Compare(b.Trigger.Priority, a.Trigger.Priority)
	})
	if len(matches) > 1 {
		e.logger.Info("workflow.match.multiple",
			"event", event, "picked", matches[0].ID,
			"picked_priority", matches[0].Trigger.Priority,
			"total", len(matches))
	}
	return matches[0]
}

// DispatchEvent finds a workflow whose trigger matches the given event and
// extraFields, then starts it seeded with vars. Returns the started workflow
// ID, or "" if no matching definition was found. Use this for external
// triggers like pr.event so the trigger conditions in the YAML stay
// authoritative instead of being bypassed by direct StartWorkflow calls.
//
// If the task already has a non-terminal workflow running, returns
// ErrWorkflowAlreadyActive and does not dispatch. Callers that intentionally
// want to replace an active workflow should use StartWorkflowWithVars.
func (e *Engine) DispatchEvent(taskID, event string, extraFields, vars map[string]string) (string, error) {
	// Serialize dispatch attempts per task to prevent concurrent callers from
	// both observing "no active workflow" and double-starting.
	e.mu.Lock()
	if _, busy := e.dispatching[taskID]; busy {
		e.mu.Unlock()
		return "", fmt.Errorf("%w: dispatch in progress", ErrWorkflowAlreadyActive)
	}
	e.dispatching[taskID] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.dispatching, taskID)
		e.mu.Unlock()
	}()

	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	if t.Workflow != nil &&
		t.Workflow.State != ExecCompleted &&
		t.Workflow.State != ExecFailed {
		return "", fmt.Errorf("%w: %s (state=%s)",
			ErrWorkflowAlreadyActive, t.Workflow.WorkflowID, t.Workflow.State)
	}
	def := e.matchWorkflow(t, event, extraFields)
	if def == nil {
		return "", nil
	}
	if err := e.StartWorkflowWithVars(taskID, def.ID, vars); err != nil {
		return "", fmt.Errorf("start %s: %w", def.ID, err)
	}
	return def.ID, nil
}
