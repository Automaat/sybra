package workflow

import (
	"cmp"
	"errors"
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
	return e.StartWorkflowFromStepWithVars(taskID, workflowID, "", vars)
}

// StartWorkflowFromStepWithVars is StartWorkflowWithVars with an optional
// explicit entry step. An empty startStepID uses the workflow's first step.
// Used by recovery flows that must resume at the interrupted step rather than
// replaying the workflow from the beginning.
func (e *Engine) StartWorkflowFromStepWithVars(taskID, workflowID, startStepID string, vars map[string]string) error {
	// startWorkflowLocked holds the `starting` marker; it is released by the
	// time this returns, so firing the completion here cannot re-enter against
	// it. This is what lets a synchronous mechanical workflow (e.g.
	// simple-task-handoff) cascade into its successor.
	comp, err := e.startWorkflowLocked(taskID, workflowID, startStepID, vars)
	if errors.Is(err, errBestOfNParked) {
		err = nil
	}
	e.fireComplete(comp)
	return err
}

// startWorkflowLocked is the marker-holding body shared by StartWorkflowWithVars
// and DispatchEvent. It returns a non-nil *CompletionInfo when the workflow
// finished synchronously within this call; the caller must hand it to
// fireComplete only AFTER releasing its own per-task marker (starting via this
// function's defer, plus dispatching for DispatchEvent) so the completion's
// cascade dispatch is not rejected as re-entrant.
func (e *Engine) startWorkflowLocked(taskID, workflowID, startStepID string, vars map[string]string) (*CompletionInfo, error) {
	e.mu.Lock()
	if _, busy := e.starting[taskID]; busy {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: start in progress", ErrWorkflowAlreadyActive)
	}
	e.starting[taskID] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.starting, taskID)
		e.mu.Unlock()
	}()
	return e.startWorkflowCore(taskID, workflowID, startStepID, vars)
}

// startWorkflowCore is the marker-agnostic body of startWorkflowLocked: build
// the Execution and run it. Split out so ReplaceWorkflow can perform a
// cancel-then-start atomically without re-acquiring e.starting — see
// ReplaceWorkflow's doc for why re-acquiring deadlocks.
func (e *Engine) startWorkflowCore(taskID, workflowID, startStepID string, vars map[string]string) (*CompletionInfo, error) {
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
		return nil, fmt.Errorf("%w: %s (state=%s)",
			ErrWorkflowAlreadyActive, t.Workflow.WorkflowID, t.Workflow.State)
	}

	def, err := e.store.Get(workflowID)
	if err != nil {
		return nil, fmt.Errorf("get workflow %s: %w", workflowID, err)
	}

	start := def.FirstStep()
	if startStepID != "" {
		start = def.StepByID(startStepID)
		if start == nil {
			return nil, fmt.Errorf("workflow %s step %s not found", workflowID, startStepID)
		}
	} else if start == nil {
		return nil, fmt.Errorf("workflow %s has no steps", workflowID)
	}

	variables := make(map[string]string, len(vars))
	maps.Copy(variables, vars)

	wfExec := &Execution{
		WorkflowID:  workflowID,
		CurrentStep: start.ID,
		State:       ExecRunning,
		Variables:   variables,
		StartedAt:   time.Now().UTC(),
	}

	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return nil, fmt.Errorf("set workflow on task: %w", err)
	}

	e.logger.Info("workflow.start", "task_id", taskID, "workflow", workflowID, "step", start.ID)
	comp, err := e.executeSteps(taskID, &def, start, wfExec)
	if errors.Is(err, errBestOfNParked) {
		return nil, errBestOfNParked
	}
	return comp, err
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
// want to replace an active workflow should use ReplaceWorkflow or
// ReplaceWorkflowForEvent.
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
	// If the matched workflow finishes synchronously (a mechanical workflow with
	// no async step), its completion must be fired only after `dispatching` is
	// cleared, or its cascade dispatch re-enters and is dropped. Register the
	// fire defer *before* the marker-delete defer so LIFO runs it afterwards.
	var completion *CompletionInfo
	defer func() { e.fireComplete(completion) }()
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
	comp, sErr := e.startWorkflowLocked(taskID, def.ID, "", vars)
	if errors.Is(sErr, errBestOfNParked) {
		sErr = nil
	}
	if sErr != nil {
		return "", fmt.Errorf("start %s: %w", def.ID, sErr)
	}
	completion = comp
	return def.ID, nil
}

// ReplaceWorkflowForEvent matches event exactly like DispatchEvent, then
// replaces the task's active workflow with the matched definition.
//
// This is for reentrant recovery paths that are already executing inside the
// workflow being replaced: they must keep trigger conditions authoritative, but
// cannot call DispatchEvent because the outer workflow start still owns the
// per-task starting marker. Callers from ordinary external event sources should
// keep using DispatchEvent so the dispatching marker serializes them.
func (e *Engine) ReplaceWorkflowForEvent(taskID, event string, extraFields, vars map[string]string, cancelReason string) (string, error) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	def := e.matchWorkflow(t, event, extraFields)
	if def == nil {
		return "", nil
	}
	if err := e.ReplaceWorkflow(taskID, cancelReason, def.ID, vars); err != nil {
		return "", fmt.Errorf("replace with %s: %w", def.ID, err)
	}
	return def.ID, nil
}

// HasActiveWorkflow reports whether the task has a non-terminal workflow
// execution attached. Returns false when the task is unknown, has no
// workflow, or its workflow has reached ExecCompleted/ExecFailed.
//
// Pre-check for callers that want to bail out early (no worktree prep,
// no audit emit) when DispatchEvent would otherwise reject with
// ErrWorkflowAlreadyActive. This is racy by construction — a workflow
// can complete or start between the check and the dispatch — so callers
// must still treat ErrWorkflowAlreadyActive from DispatchEvent as
// benign. Used by pr-monitor to suppress layered re-dispatches while a
// pr-fix run is in flight, including the gap between an agent
// finishing and the workflow's verify_commits/link_pr steps advancing.
func (e *Engine) HasActiveWorkflow(taskID string) bool {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return false
	}
	if t.Workflow == nil {
		return false
	}
	return t.Workflow.State != ExecCompleted && t.Workflow.State != ExecFailed
}

// CancelWorkflow terminates a task's active workflow without running any
// remaining steps. Stops in-flight agents for the task, marks the workflow
// ExecCompleted with the cancellation reason recorded in variables, and
// clears CurrentStep so ResumeStalled stops re-dispatching.
//
// No-op when the task has no workflow or its workflow already terminated.
// Returns the prior current step ID for the caller's log line; empty when
// the workflow had already ended.
//
// Does NOT fire OnComplete — cascading the cancel into the next workflow
// (e.g. simple-task-review on ready-review) is rarely what the caller
// wants, and pr-monitor wants the task to fall back to its prior state.
func (e *Engine) CancelWorkflow(taskID, reason string) (string, error) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return "", err
	}
	if t.Workflow == nil {
		return "", nil
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		return "", nil
	}

	priorStep := t.Workflow.CurrentStep

	// Stop any in-flight agents first so their completion callback runs
	// against the about-to-be-terminal workflow (HandleAgentComplete's
	// terminal guard at engine_events.go:128 turns it into a no-op).
	e.agents.StopAgentsForTask(taskID, "")

	now := time.Now().UTC()
	wfExec := t.Workflow
	wfExec.State = ExecCompleted
	wfExec.CompletedAt = &now
	wfExec.CurrentStep = ""
	if reason != "" {
		wfExec.SetVar("cancel_reason", reason)
	}
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return priorStep, err
	}
	e.logger.Info("workflow.cancelled",
		"task_id", taskID, "workflow", wfExec.WorkflowID,
		"step", priorStep, "reason", reason)
	return priorStep, nil
}

// ReplaceWorkflow atomically cancels the task's current active workflow
// (recording cancelReason) and starts newWorkflowID with vars in its place.
//
// This exists because CancelWorkflow followed by a separate
// StartWorkflowWithVars call deadlocks when both run inside the same call
// stack that is already executing a step of the workflow being replaced —
// e.g. divergence recovery invoked synchronously from create_pr's
// pushTaskBranch. That stack already holds e.starting[taskID] (set by the
// enclosing startWorkflowLocked/DispatchEvent call for the workflow currently
// executing), so the nested StartWorkflowWithVars always observes the marker
// busy and fails with ErrWorkflowAlreadyActive ("start in progress") — a
// guaranteed reentrant failure every time, not a transient race.
//
// Safe to call from that nested position: the outer call already serializes
// concurrent starts for taskID, so ReplaceWorkflow can perform the cancel+
// start under that same guarantee instead of re-acquiring e.starting.
func (e *Engine) ReplaceWorkflow(taskID, cancelReason, newWorkflowID string, vars map[string]string) error {
	if _, err := e.CancelWorkflow(taskID, cancelReason); err != nil {
		return fmt.Errorf("cancel prior workflow: %w", err)
	}
	comp, err := e.startWorkflowCore(taskID, newWorkflowID, "", vars)
	if errors.Is(err, errBestOfNParked) {
		err = nil
	}
	e.fireComplete(comp)
	return err
}
