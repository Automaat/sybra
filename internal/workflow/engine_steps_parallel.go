package workflow

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// execParallel spawns every child of a `parallel` block concurrently, all
// against the shared task worktree. Children are headless run_agent steps
// (validated at definition load time); planning is read-only so contention
// on the worktree is benign. The parent step advances to its `next` only
// after every child has terminated; per-child completions are routed
// through AdvanceStep via the existing agentSteps mapping.
func (e *Engine) execParallel(taskID string, def *Definition, step *Step, wfExec *Execution, ctx TemplateContext) error {
	if len(step.Parallel) < 2 {
		return fmt.Errorf("parallel step %q has fewer than 2 children", step.ID)
	}

	// Resume-safe: a re-entry into the same parallel parent (e.g. after a
	// process restart) finds the existing record and skips children that
	// already completed. We treat any non-pending child as already
	// dispatched; the missing pending children get re-spawned below.
	if wfExec.ParallelInflight == nil {
		wfExec.ParallelInflight = make(map[string]*ParallelChildren)
	}
	rec, exists := wfExec.ParallelInflight[step.ID]
	if !exists {
		rec = &ParallelChildren{
			ParentStepID: step.ID,
			StartedAt:    time.Now().UTC(),
			Children:     make(map[string]*ChildStatus, len(step.Parallel)),
		}
		for i := range step.Parallel {
			c := &step.Parallel[i]
			rec.Children[c.ID] = &ChildStatus{Status: "pending"}
		}
		wfExec.ParallelInflight[step.ID] = rec
	}

	// Stop stale agents from prior steps once before spawning the fan-out.
	// Doing this inside the per-child loop would kill the child we just
	// spawned for the previous iteration.
	e.agents.StopAgentsForTask(taskID, "")

	// Persist the inflight record before spawning so an agent that
	// completes mid-loop finds the parent record on AdvanceStep.
	wfExec.State = ExecWaiting
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}

	dir := wfExec.Variables[WorkflowVarDir]
	for i := range step.Parallel {
		child := &step.Parallel[i]
		status := rec.Children[child.ID]
		if status == nil {
			status = &ChildStatus{Status: "pending"}
			rec.Children[child.ID] = status
		}
		// Skip children that already terminated in a previous run (resume).
		if status.Status == "completed" || status.Status == "failed" {
			continue
		}
		if err := e.spawnParallelChild(taskID, step, child, wfExec, ctx, dir, status); err != nil {
			e.logger.Error("workflow.parallel.spawn", "task_id", taskID, "parent", step.ID, "child", child.ID, "err", err)
			// Mark this child failed up front; AdvanceStep treats failed
			// children consistently with the retry path.
			status.Status = "failed"
			status.Output = "spawn failed: " + err.Error()
		}
	}

	// If every child terminated at spawn time (all failed, or a resume found
	// every slot already terminal), no agent will fire HandleAgentComplete to
	// drive advanceParallelChild. Advance the parent synchronously instead so
	// the workflow doesn't deadlock in state=waiting.
	if rec.AllChildrenDone() {
		return e.finalizeParallelParent(taskID, def, step, wfExec)
	}

	return e.tasks.SetWorkflow(taskID, wfExec)
}

// spawnParallelChild spawns one child agent of a parallel block. Mirrors
// the body of execRunAgent but skips StopAgentsForTask (that's done once
// at the parent level) and writes results into the ChildStatus slot
// instead of mutating wfExec.State directly.
func (e *Engine) spawnParallelChild(taskID string, parent, child *Step, wfExec *Execution, parentCtx TemplateContext, dir string, status *ChildStatus) error {
	// Render the child prompt with a context that points at the child step
	// so {{.Step.ID}} et al. resolve correctly inside the prompt template.
	childCtx := parentCtx
	childCtx.Step = *child
	prompt, err := RenderTemplate(child.Config.Prompt, childCtx)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	mode := child.Config.Mode
	if strings.Contains(mode, "{{") {
		if rendered, rErr := RenderTemplate(mode, childCtx); rErr == nil {
			mode = rendered
		}
	}
	if mode == "" {
		mode = "headless"
	}
	model := child.Config.Model
	if model == "" {
		model = "sonnet"
	}

	provider := resolveProvider(child.Config.Provider, wfExec, e.agents.DefaultProvider())
	if provider != "" && !providerAvailable(provider) {
		e.logger.Warn("workflow.parallel.cross-provider.fallback", "child", child.ID, "wanted", provider, "reason", "CLI not found")
		provider = ""
	}

	// Headless one-shot: parallel children must terminate so the parent
	// can advance. Interactive/wait_for_status children are not supported
	// here (validated at definition load time would be the proper place;
	// guard here defensively).
	if mode == "interactive" {
		return fmt.Errorf("parallel child %q: interactive mode not supported", child.ID)
	}
	oneShot := false

	// Hold e.mu across StartAgent so HandleAgentComplete (which acquires
	// e.mu via lookupAgentStep) cannot race past the agentSteps registration.
	// Fast-exiting agents (e.g. fail_exit in tests) can otherwise complete
	// before agentSteps is populated, causing the wrong step to advance.
	e.mu.Lock()
	agentID, err := e.agents.StartAgent(taskID, child.Config.Role, mode, model, provider, prompt, dir, child.Config.AllowedTools, child.Config.NeedsWorktree, oneShot)
	if err != nil {
		e.mu.Unlock()
		return fmt.Errorf("start agent: %w", err)
	}
	// agentSteps key uses the *child* step ID. StepByID recurses into
	// Parallel children so the lookup in lookupAgentStep / AdvanceStep
	// returns the right step config.
	e.agentSteps[agentID] = agentEntry{taskID: taskID, stepID: child.ID}
	e.mu.Unlock()

	status.AgentID = agentID
	status.Provider = provider
	status.Status = "pending"
	e.logger.Info("workflow.parallel.spawn",
		"task_id", taskID, "parent", parent.ID, "child", child.ID,
		"role", child.Config.Role, "agent_id", agentID, "provider", provider)
	return nil
}

// advanceParallelChild records one child's completion inside its parent
// `parallel` block. Per-child retry: a failed child gets re-spawned up to
// child.Config.MaxRetries times before terminating with status=failed.
// The parent step's StepRecord + Next-evaluation only fire after every
// child has terminated. Aggregate parent status: completed iff every
// child completed; failed otherwise.
func (e *Engine) advanceParallelChild(taskID string, def *Definition, parent, child *Step, wfExec *Execution, output StepOutput) error {
	rec := wfExec.ParallelInflight[parent.ID]
	if rec == nil {
		// Parent record was cleared (e.g. another late callback after the
		// parent already advanced). Treat as a stale callback.
		e.logger.Debug("workflow.parallel.stale", "task_id", taskID, "parent", parent.ID, "child", child.ID)
		return nil
	}
	status := rec.Children[child.ID]
	if status == nil {
		status = &ChildStatus{Status: "pending"}
		rec.Children[child.ID] = status
	}

	// Per-child retry: only failures count toward MaxRetries; each retry
	// re-spawns the child agent on the shared worktree.
	if output.Status == "failed" && child.Config.MaxRetries > 0 && status.Retries < child.Config.MaxRetries {
		status.Retries++
		status.Status = "pending"
		status.Output = output.Output
		e.logger.Info("workflow.parallel.retry",
			"task_id", taskID, "parent", parent.ID, "child", child.ID,
			"attempt", status.Retries, "max", child.Config.MaxRetries)
		if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
			return err
		}
		// Re-spawn just this child. Reuse the parent's template context
		// rendered fresh from the latest task state.
		t, gErr := e.tasks.GetTask(taskID)
		if gErr != nil {
			return gErr
		}
		dir := wfExec.Variables[WorkflowVarDir]
		ctx := TemplateContext{
			Task:     t,
			Step:     *parent,
			Vars:     wfExec.Variables,
			Workflow: wfExec,
		}
		if spawnErr := e.spawnParallelChild(taskID, parent, child, wfExec, ctx, dir, status); spawnErr != nil {
			status.Status = "failed"
			status.Output = "respawn failed: " + spawnErr.Error()
			e.logger.Error("workflow.parallel.respawn", "task_id", taskID, "parent", parent.ID, "child", child.ID, "err", spawnErr)
		}
		return e.tasks.SetWorkflow(taskID, wfExec)
	}

	// Terminal status — update slot.
	status.AgentID = output.AgentID
	status.Provider = output.Provider
	status.Status = output.Status
	status.Output = truncate(output.Output, 4000)

	// Wait for the rest of the cohort.
	if !rec.AllChildrenDone() {
		e.logger.Debug("workflow.parallel.child-done",
			"task_id", taskID, "parent", parent.ID, "child", child.ID,
			"status", status.Status)
		return e.tasks.SetWorkflow(taskID, wfExec)
	}

	return e.finalizeParallelParent(taskID, def, parent, wfExec)
}

// finalizeParallelParent collapses a parallel block whose children have all
// reached terminal status into a single parent StepRecord and advances via
// the parent's Next. Called from advanceParallelChild on the last
// completion, and from execParallel when every child failed at spawn time
// (no agent ever ran → no completion callback will fire).
func (e *Engine) finalizeParallelParent(taskID string, def *Definition, parent *Step, wfExec *Execution) error {
	rec := wfExec.ParallelInflight[parent.ID]
	if rec == nil {
		return nil
	}

	parentStatus := "completed"
	if rec.AnyChildFailed() {
		parentStatus = "failed"
	}
	parentOutput := summarizeChildOutputs(rec)

	// Clear the inflight record before recording the parent step so a stale
	// late callback can't re-enter advanceParallelChild for this parent.
	delete(wfExec.ParallelInflight, parent.ID)

	now := time.Now().UTC()
	wfExec.RecordStep(StepRecord{
		StepID:    parent.ID,
		Status:    parentStatus,
		Output:    truncate(parentOutput, 4000),
		StartedAt: rec.StartedAt,
		EndedAt:   now,
	})
	if parentOutput != "" {
		wfExec.SetVar("step."+parent.ID+".output", truncate(parentOutput, 2000))
	}

	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	t.Workflow = wfExec
	nextStep, err := e.resolveNext(taskID, def, parent, wfExec, t)
	if err != nil {
		return err
	}
	if nextStep == nil {
		return nil
	}
	e.logger.Info("workflow.parallel.advance", "task_id", taskID, "from", parent.ID, "to", nextStep.ID, "status", parentStatus)
	return e.executeSteps(taskID, def, nextStep, wfExec)
}

// summarizeChildOutputs renders a compact "child=status" summary that
// downstream steps (e.g. converge_plans) can reference via vars.step.<parent>.output.
func summarizeChildOutputs(rec *ParallelChildren) string {
	if rec == nil {
		return ""
	}
	parts := make([]string, 0, len(rec.Children))
	for id, c := range rec.Children {
		parts = append(parts, fmt.Sprintf("%s=%s", id, c.Status))
	}
	slices.Sort(parts) // deterministic for test assertions
	return strings.Join(parts, ", ")
}
