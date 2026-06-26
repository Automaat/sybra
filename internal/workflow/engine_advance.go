package workflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AdvanceStep is called when an async step completes. It records the result,
// evaluates transitions, and executes the next step.
//
// No-ops (returns nil) when the workflow is already in a terminal state
// (completed/failed) or when the current step is empty. This prevents stale
// agent completions — e.g. agents spawned outside the workflow, or a
// double-delivered callback — from triggering "step not found" errors that
// would otherwise spam the log and re-persist the task file on every hit.
func (e *Engine) AdvanceStep(taskID string, output StepOutput) error {
	e.acquireInflight(taskID) // blocks until any concurrent advance releases

	// Use an idempotent release so we can unlock before executeSteps while
	// still having a defer as a safety net for early-return paths. Releasing
	// before executeSteps is required: execRunAgent calls StopAgentsForTask
	// which may wait for completing agents; those agents' onComplete callbacks
	// call AdvanceStep and block on acquireInflight — holding the lock through
	// executeSteps would deadlock that sequence.
	released := false
	release := func() {
		if !released {
			released = true
			e.releaseInflight(taskID)
		}
	}
	defer release()

	ctx, skip, err := e.loadAdvanceContext(taskID, output)
	if err != nil || skip {
		return err
	}
	wfExec, def, currentStep := ctx.WfExec, ctx.Def, ctx.Step

	// Parallel-child completion: route to the child-aware path. The parent
	// step's record + transitions are emitted only after every child has
	// terminated, so we never go through the single-step record path here.
	if ctx.ParallelParent != nil {
		comp, pErr := e.advanceParallelChild(taskID, &def, ctx.ParallelParent, currentStep, wfExec, output)
		// Release inflight before the completion callback so its cascade
		// dispatch never runs under the held lock (matches the paths below).
		release()
		e.fireComplete(comp)
		return pErr
	}

	// Record step completion.
	now := time.Now().UTC()
	wfExec.RecordStep(StepRecord{
		StepID:    output.StepID,
		Status:    output.Status,
		Output:    truncate(output.Output, 4000),
		AgentID:   output.AgentID,
		Provider:  output.Provider,
		StartedAt: now,
		EndedAt:   now,
	})
	if output.Output != "" {
		wfExec.SetVar("step."+output.StepID+".output", truncate(output.Output, 2000))
		// Extract the adversarial test verdict from the UNtruncated output and
		// stash it in a tiny dedicated var. The verdict marker sits on the final
		// line and would otherwise be lost to the 2000-byte prefix truncation
		// above whenever a thorough test-runner writes a long summary. No-op
		// (empty) for every non-test step. See route_test_result.
		if v := extractTestVerdict(output.Output); v != "" {
			wfExec.SetVar("step."+output.StepID+".verdict", v)
		}
	}

	// Retry failed steps if max_retries configured and not exhausted.
	if output.Status == "failed" && currentStep.Config.MaxRetries > 0 {
		retries := wfExec.CountStep(output.StepID)
		if retries <= currentStep.Config.MaxRetries {
			e.logger.Info("workflow.retry", "task_id", taskID, "step", output.StepID,
				"attempt", retries, "max", currentStep.Config.MaxRetries)
			if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
				return err
			}
			release()
			comp, sErr := e.executeSteps(taskID, &def, currentStep, wfExec)
			e.fireComplete(comp)
			return sErr
		}
		e.logger.Warn("workflow.retry.exhausted", "task_id", taskID, "step", output.StepID,
			"attempts", retries)
	}

	// Mark task reviewed after a review-role step succeeds.
	// Persisted so a re-triggered workflow run skips code_review (idempotent).
	if currentStep.Config.Role == "review" && output.Status == "completed" {
		if mErr := e.tasks.MarkTaskReviewed(taskID); mErr != nil {
			e.logger.Warn("workflow.mark-reviewed.failed", "task_id", taskID, "err", mErr)
		}
	}

	// Re-read task for latest state (agent may have changed tags/status).
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	t.Workflow = wfExec

	nextStep, comp, err := e.resolveNext(taskID, &def, currentStep, wfExec, t)
	if err != nil {
		return err
	}
	if nextStep == nil {
		// Release the inflight lock before the completion callback so its
		// cascade dispatch doesn't re-enter AdvanceStep against a held lock.
		release()
		e.fireComplete(comp)
		return nil // workflow completed
	}

	e.logger.Info("workflow.advance", "task_id", taskID, "from", output.StepID, "to", nextStep.ID)
	release()
	comp, sErr := e.executeSteps(taskID, &def, nextStep, wfExec)
	e.fireComplete(comp)
	return sErr
}

// acquireInflight serializes AdvanceStep for a task. Blocks (rather than
// returning false) so simultaneous parallel-child completions from
// different agent goroutines are processed sequentially instead of one
// silently being dropped. Always returns true; the bool return is kept
// so callers can preserve the "skip on already-advancing" log line.
//
// Re-entry within the same goroutine is not supported — every AdvanceStep
// path defers releaseInflight before any callback that could re-enter.
func (e *Engine) acquireInflight(taskID string) bool {
	mu := e.taskInflightMutex(taskID)
	mu.Lock()
	return true
}

// releaseInflight unlocks the per-task advance mutex.
func (e *Engine) releaseInflight(taskID string) {
	mu := e.taskInflightMutex(taskID)
	mu.Unlock()
}

// taskInflightMutex returns the lazily-initialized per-task mutex used by
// acquire/releaseInflight. Old taskInflightMutex entries linger for the
// life of the process; tasks with hundreds of millions of IDs would leak
// memory, but task IDs are bounded by the human workload so this is fine.
func (e *Engine) taskInflightMutex(taskID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	mu, ok := e.inflightMutexes[taskID]
	if !ok {
		mu = &sync.Mutex{}
		e.inflightMutexes[taskID] = mu
	}
	return mu
}

// advanceContext bundles everything AdvanceStep needs to act on a single
// step completion. ParallelParent is non-nil when the resolved Step is a
// child of an in-flight `parallel` block.
type advanceContext struct {
	WfExec         *Execution
	Def            Definition
	Step           *Step
	ParallelParent *Step
}

// loadAdvanceContext validates and resolves the state needed by AdvanceStep.
// Returns skip=true (with nil error and ctx={}) for every legitimate no-op
// path: a terminal workflow, an empty step ID, a stale step (the
// ResumeStalled-race duplicate-agent guard), or an unexpected agent callback
// hitting a wait_human step without a human_action var set
// (defense-in-depth).
//
// When the workflow's current step is a `parallel` block and `output.StepID`
// names one of its children, ctx.Step is the *child* (so retry counters and
// ImportSidecar lookups operate on the child's config) and
// ctx.ParallelParent is non-nil.
func (e *Engine) loadAdvanceContext(taskID string, output StepOutput) (advanceContext, bool, error) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return advanceContext{}, false, err
	}
	if t.Workflow == nil {
		return advanceContext{}, false, fmt.Errorf("task %s has no active workflow", taskID)
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		e.logger.Debug("workflow.advance.skip",
			"task_id", taskID, "reason", "workflow_terminal",
			"state", string(t.Workflow.State), "step_id", output.StepID)
		return advanceContext{}, true, nil
	}
	if output.StepID == "" {
		e.logger.Debug("workflow.advance.skip",
			"task_id", taskID, "reason", "empty_step_id",
			"state", string(t.Workflow.State))
		return advanceContext{}, true, nil
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return advanceContext{}, false, err
	}

	// Stale-step / parallel-child check: the output step must either match
	// the current step exactly, or be a child of the current step when the
	// current step is a `parallel` block. Anything else is a stale callback.
	currentStep := def.StepByID(t.Workflow.CurrentStep)
	if currentStep == nil {
		return advanceContext{}, false, fmt.Errorf("step %s not found in workflow %s", t.Workflow.CurrentStep, def.ID)
	}
	var parallelParent *Step
	resolvedStep := currentStep
	if output.StepID != t.Workflow.CurrentStep {
		if currentStep.Type == StepParallel && parallelHasChild(currentStep, output.StepID) {
			parallelParent = currentStep
			resolvedStep = def.StepByID(output.StepID) // child (StepByID recurses)
			if resolvedStep == nil {
				return advanceContext{}, false, fmt.Errorf("parallel child %s not found in workflow %s", output.StepID, def.ID)
			}
		} else {
			e.logger.Debug("workflow.advance.skip",
				"task_id", taskID, "reason", "stale_step",
				"output_step", output.StepID, "current_step", t.Workflow.CurrentStep,
				"agent_id", output.AgentID)
			return advanceContext{}, true, nil
		}
	}

	if resolvedStep.Type == StepWaitHuman && output.AgentID != "" {
		if _, set := t.Workflow.Variables["human_action"]; !set {
			e.logger.Debug("workflow.advance.skip",
				"task_id", taskID, "reason", "wait_human_no_action",
				"step", output.StepID, "agent_id", output.AgentID)
			return advanceContext{}, true, nil
		}
	}

	return advanceContext{
		WfExec:         t.Workflow,
		Def:            def,
		Step:           resolvedStep,
		ParallelParent: parallelParent,
	}, false, nil
}

// parallelHasChild reports whether `parent` is a parallel block that lists
// `childID` among its direct children.
func parallelHasChild(parent *Step, childID string) bool {
	if parent == nil || parent.Type != StepParallel {
		return false
	}
	for i := range parent.Parallel {
		if parent.Parallel[i].ID == childID {
			return true
		}
	}
	return false
}

// CycleError is returned when executeSteps detects a cycle in the synchronous
// step chain — the same step ID was visited twice without an async step
// (run_agent, wait_human) breaking the loop.
type CycleError struct {
	StepID string
	// At is the iteration index at which the cycle was detected (0-based).
	At int
	// FirstAt is the iteration index at which the step was first visited.
	FirstAt int
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("workflow cycle detected: step %q revisited at iteration %d (first seen at %d)",
		e.StepID, e.At, e.FirstAt)
}

// executeSteps iterates through synchronous steps until it hits an async step
// (run_agent, wait_human) or the workflow ends. This avoids recursive calls
// between executeStep/AdvanceStep that caused inflight guard deadlocks.
// executeSteps drives synchronous steps until the workflow either reaches an
// async step (run_agent/parallel/wait_human — returns nil completion) or ends
// (returns a non-nil *CompletionInfo). The completion is returned rather than
// fired here so marker-holding callers (StartWorkflowWithVars, DispatchEvent,
// ResumeStalled) can fireComplete it only after their per-task marker is
// released; non-marker callers fireComplete it immediately.
func (e *Engine) executeSteps(taskID string, def *Definition, step *Step, wfExec *Execution) (*CompletionInfo, error) {
	visited := make(map[string]int) // stepID → first-seen iteration index
	for i := range maxSyncSteps {
		t, err := e.tasks.GetTask(taskID)
		if err != nil {
			return nil, err
		}

		// Snapshot the execution for the template context so that clearing
		// the Recovered flag below doesn't affect what the template sees.
		execSnap := *wfExec
		ctx := TemplateContext{
			Task:     t,
			Step:     *step,
			Prev:     wfExec.LastRecord(),
			Vars:     wfExec.Variables,
			Project:  nil,
			Workflow: &execSnap,
		}

		// Consume the recovery flag: it applies only to the step being
		// dispatched here. Clear and persist before spawning the agent so
		// subsequent HandleAgentComplete reloads don't see a stale flag.
		if wfExec.Recovered {
			wfExec.Recovered = false
			if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
				return nil, err
			}
		}

		// Async steps: execute and return. Callback (HandleAgentComplete/HandleHumanAction)
		// will call AdvanceStep later.
		switch step.Type {
		case StepRunAgent:
			return nil, e.execRunAgent(taskID, step, wfExec, ctx)
		case StepParallel:
			return e.execParallel(taskID, def, step, wfExec, ctx)
		case StepWaitHuman:
			return nil, e.execWaitHuman(taskID, step, wfExec)
		case StepSetStatus, StepCondition, StepShell, StepEnsurePRClosesIssue, StepRerequestReview, StepVerifyCommits, StepLinkPRAndReview, StepEvaluate, StepRequireSidecar, StepValidatePlan, StepTriageReview, StepDetectTampering, StepVerifyChecks, StepRouteTestResult:
			// handled below as sync steps
		default:
			return nil, fmt.Errorf("unknown step type %q", step.Type)
		}

		// Detect cycles: a sync step revisited without an async break means
		// the workflow loops forever. Return a CycleError instead of hitting
		// the generic maxSyncSteps limit.
		if firstAt, seen := visited[step.ID]; seen {
			return nil, &CycleError{StepID: step.ID, At: i, FirstAt: firstAt}
		}
		visited[step.ID] = i

		// Sync steps: execute, record result, resolve next, loop.
		output, execErr := e.execSyncStep(taskID, step, wfExec, ctx, t)
		if execErr != nil {
			// The step parked the workflow in ExecWaiting (it persisted the new
			// CurrentStep/State itself). Stop without recording/advancing/
			// completing: completing here would fire the status-change cascade.
			// ResumeStalled re-drives the re-armed run_agent step once idle.
			if errors.Is(execErr, errStepParked) {
				var parkedNoCompletion *CompletionInfo
				return parkedNoCompletion, nil
			}
			return nil, execErr
		}

		now := time.Now().UTC()
		wfExec.RecordStep(StepRecord{
			StepID:    step.ID,
			Status:    output.Status,
			Output:    truncate(output.Output, 4000),
			StartedAt: now,
			EndedAt:   now,
		})
		if output.Output != "" {
			wfExec.SetVar("step."+step.ID+".output", truncate(output.Output, 2000))
		}

		// Re-read task for latest state (set_status changes task).
		t, err = e.tasks.GetTask(taskID)
		if err != nil {
			return nil, err
		}
		t.Workflow = wfExec

		nextStep, comp, nErr := e.resolveNext(taskID, def, step, wfExec, t)
		if nErr != nil {
			return nil, nErr
		}
		if nextStep == nil {
			return comp, nil // workflow completed; caller fires onComplete after marker release
		}

		e.logger.Info("workflow.advance", "task_id", taskID, "from", step.ID, "to", nextStep.ID)
		step = nextStep
	}
	return nil, fmt.Errorf("workflow exceeded max sync step depth (%d)", maxSyncSteps)
}

// execSyncStep dispatches to a synchronous step handler and returns its output.
func (e *Engine) execSyncStep(taskID string, step *Step, wfExec *Execution, ctx TemplateContext, t TaskInfo) (StepOutput, error) {
	switch step.Type {
	case StepSetStatus:
		return e.execSetStatus(taskID, step)
	case StepCondition:
		return e.execCondition(step, wfExec, t)
	case StepShell:
		return e.execShell(step, ctx)
	case StepEnsurePRClosesIssue:
		return e.execEnsurePRClosesIssue(taskID, step, t)
	case StepRerequestReview:
		return e.execRerequestReview(taskID, step, t)
	case StepVerifyCommits:
		return e.execVerifyCommits(taskID, step, wfExec, t)
	case StepLinkPRAndReview:
		return e.execLinkPRAndReview(taskID, step, wfExec, t)
	case StepEvaluate:
		return e.execEvaluate(taskID, step, wfExec, t)
	case StepRequireSidecar:
		return e.execRequireSidecar(taskID, step, t)
	case StepValidatePlan:
		return e.execValidatePlan(taskID, step, t)
	case StepTriageReview:
		return e.execTriageReview(taskID, step, t)
	case StepDetectTampering:
		return e.execDetectTampering(taskID, step, t)
	case StepVerifyChecks:
		return e.execVerifyChecks(taskID, step, t)
	case StepRouteTestResult:
		return e.execRouteTestResult(taskID, step, wfExec, t)
	default:
		return StepOutput{}, fmt.Errorf("unknown step type %q", step.Type)
	}
}

// resolveNext evaluates transitions and returns the next step, or nil if the
// workflow ends. When the workflow ends it returns a non-nil *CompletionInfo;
// the caller must hand it to fireComplete *after* releasing any per-task start
// marker (see fireComplete). resolveNext deliberately does NOT invoke
// e.onComplete itself: it runs inside DispatchEvent/StartWorkflowWithVars while
// the dispatching/starting marker is held, so a cascade dispatched here would
// be rejected as re-entrant (the bug that left synchronous mechanical workflows
// — e.g. simple-task-handoff — never starting their successor).
func (e *Engine) resolveNext(taskID string, def *Definition, current *Step, wfExec *Execution, t TaskInfo) (*Step, *CompletionInfo, error) {
	fields := taskFields(t)
	for k, v := range wfExec.Variables {
		fields["vars."+k] = v
	}
	if wfExec.Recovered {
		fields["vars.recovered"] = "true"
	}

	nextID, tErr := ResolveTransition(current.Next, fields)
	if tErr != nil {
		e.logger.Error("workflow.transition.failed", "task_id", taskID, "step", current.ID, "err", tErr)
		wfExec.State = ExecFailed
		_ = e.tasks.SetWorkflow(taskID, wfExec)
		return nil, nil, tErr
	}

	if nextID == "" {
		now := time.Now().UTC()
		wfExec.State = ExecCompleted
		wfExec.CompletedAt = &now
		wfExec.CurrentStep = ""
		e.logger.Info("workflow.completed", "task_id", taskID, "workflow", def.ID)
		if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
			return nil, nil, err
		}
		return nil, &CompletionInfo{
			TaskID:     taskID,
			WorkflowID: def.ID,
			Variables:  wfExec.Variables,
		}, nil
	}

	nextStep := def.StepByID(nextID)
	if nextStep == nil {
		return nil, nil, fmt.Errorf("next step %s not found in workflow %s", nextID, def.ID)
	}

	wfExec.CurrentStep = nextStep.ID
	wfExec.State = ExecRunning
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return nil, nil, err
	}
	return nextStep, nil, nil
}

// maxCascadeDepth bounds how many workflows may chain synchronously off a
// single task before the engine refuses to cascade further. The completion
// callback runs the next workflow's DispatchEvent inline, so an all-mechanical
// loop (e.g. two set_status workflows that ping-pong a task between two
// non-terminal statuses) would otherwise recurse on the stack until it
// overflows. Production builtins never approach this — every successor reaches
// an async run_agent/wait_human step that breaks the chain — so the bound only
// fires on a misconfigured definition set, which it converts into a logged
// error instead of a crash.
const maxCascadeDepth = 64

// fireComplete invokes the workflow-completion callback for a synchronously
// finished workflow. Callers MUST invoke it only after releasing any per-task
// start marker (the dispatching/starting maps), so the callback's cascade
// DispatchEvent isn't rejected as re-entrant against the workflow that just
// finished. A nil completion (workflow did not finish in this call) is a no-op.
func (e *Engine) fireComplete(c *CompletionInfo) {
	if c == nil || e.onComplete == nil {
		return
	}

	e.mu.Lock()
	e.cascadeDepth[c.TaskID]++
	depth := e.cascadeDepth[c.TaskID]
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		if e.cascadeDepth[c.TaskID] <= 1 {
			delete(e.cascadeDepth, c.TaskID)
		} else {
			e.cascadeDepth[c.TaskID]--
		}
		e.mu.Unlock()
	}()

	if depth > maxCascadeDepth {
		e.logger.Error("workflow.cascade.depth-exceeded",
			"task_id", c.TaskID, "workflow", c.WorkflowID, "depth", depth)
		return
	}

	e.onComplete(*c)
}

func taskFields(t TaskInfo) map[string]string {
	fields := map[string]string{
		"task.id":                      t.ID,
		"task.title":                   t.Title,
		"task.status":                  t.Status,
		"task.tags":                    strings.Join(t.Tags, ","),
		"task.agent_mode":              t.AgentMode,
		"task.project_id":              t.ProjectID,
		"task.handoff_source_provider": t.HandoffSourceProvider,
		"task.branch":                  t.Branch,
		"task.reviewed":                strconv.FormatBool(t.Reviewed),
	}
	if t.PRNumber > 0 {
		fields["task.pr_number"] = strconv.Itoa(t.PRNumber)
	}
	return fields
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n... (truncated)"
}
