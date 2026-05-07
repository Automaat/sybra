package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// importSidecarIfConfigured reads the file the agent produced (template
// rendered from step.config.import_sidecar.from) and stores its content
// as the configured task sidecar. Called from HandleAgentComplete on
// success so the host — which can write anywhere — closes the gap when
// the agent's sandbox blocks ~/.sybra/tasks/. Errors are logged, not
// returned: the require_sidecar guard surfaces an empty sidecar by
// flipping the task to human-required, which is the correct UX.
func (e *Engine) importSidecarIfConfigured(taskID, stepID string, info TaskInfo) {
	if info.Workflow == nil {
		return
	}
	def, err := e.store.Get(info.Workflow.WorkflowID)
	if err != nil {
		return
	}
	step := def.StepByID(stepID)
	if step == nil || step.Type != StepRunAgent || step.Config.ImportSidecar == nil {
		return
	}
	cfg := step.Config.ImportSidecar
	path, rErr := RenderTemplate(cfg.From, TemplateContext{
		Task:     info,
		Step:     *step,
		Vars:     info.Workflow.Variables,
		Workflow: info.Workflow,
	})
	if rErr != nil {
		e.logger.Warn("workflow.import-sidecar.render", "task_id", taskID, "step", stepID, "err", rErr)
		return
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		e.logger.Warn("workflow.import-sidecar.read", "task_id", taskID, "step", stepID, "path", path, "err", readErr)
		return
	}
	// Convention: a bare "plan_draft" kind is auto-namespaced by the step
	// ID so a single workflow can fan out to N parallel planners without
	// each having to spell out a unique kind. The result lands in the
	// PlanDraftStore under name=<step ID>.
	kind := cfg.Kind
	if kind == "plan_draft" {
		kind = "plan_draft." + stepID
	}
	if writeErr := e.tasks.WriteSidecar(taskID, kind, string(content)); writeErr != nil {
		e.logger.Error("workflow.import-sidecar.write", "task_id", taskID, "step", stepID, "kind", kind, "err", writeErr)
		return
	}
	e.logger.Info("workflow.import-sidecar", "task_id", taskID, "step", stepID, "kind", kind, "path", path, "bytes", len(content))
}

func (e *Engine) execRunAgent(taskID string, step *Step, wfExec *Execution, ctx TemplateContext) error {
	prompt, err := RenderTemplate(step.Config.Prompt, ctx)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	// Reuse a live agent if configured and one exists for this role.
	if step.Config.ReuseAgent {
		if agentID, found := e.agents.FindRunningAgentForRole(taskID, step.Config.Role); found {
			if sendErr := e.agents.SendPrompt(agentID, prompt); sendErr != nil {
				e.logger.Warn("workflow.reuse-agent.send-failed", "task_id", taskID, "agent_id", agentID, "err", sendErr)
				e.agents.StopAgentsForTask(taskID, step.Config.Role)
			} else {
				wfExec.State = ExecWaiting
				e.logger.Info("workflow.reuse-agent", "task_id", taskID, "step", step.ID, "agent_id", agentID)
				return e.tasks.SetWorkflow(taskID, wfExec)
			}
		}
	}

	mode := step.Config.Mode
	if strings.Contains(mode, "{{") {
		rendered, rErr := RenderTemplate(mode, ctx)
		if rErr == nil {
			mode = rendered
		}
	}
	if mode == "" {
		mode = "headless"
	}

	model := step.Config.Model
	if model == "" {
		model = "sonnet"
	}

	provider := resolveProvider(step.Config.Provider, wfExec, e.agents.DefaultProvider())
	if provider != "" && !providerAvailable(provider) {
		e.logger.Warn("workflow.cross-provider.fallback", "wanted", provider, "reason", "CLI not found")
		provider = ""
	}

	dir := wfExec.Variables[WorkflowVarDir]

	// Stop stale agents left over from earlier workflow steps (e.g. an
	// interactive plan agent with reuse_agent that outlived plan approval).
	// Empty role = stop all roles for this task.
	e.agents.StopAgentsForTask(taskID, "")

	// Interactive agents that aren't meant to persist across turns (no
	// reuse_agent, no wait_for_status) must signal completion via process
	// exit. OneShot tells the runner to close stdin after the first result
	// event so claude exits and onComplete fires, unblocking the next step
	// (e.g. evaluate). Without this, the workflow stalls on implement forever.
	oneShot := mode == "interactive" && !step.Config.ReuseAgent && step.Config.WaitForStatus == ""
	// Hold e.mu across StartAgent + registration so HandleAgentComplete's
	// lookupAgentStep (which acquires e.mu) cannot fire before the agentID
	// is in the map — closing the race between agent spawn and step tracking.
	e.mu.Lock()
	agentID, err := e.agents.StartAgent(taskID, step.Config.Role, mode, model, provider, prompt, dir, step.Config.AllowedTools, step.Config.NeedsWorktree, oneShot)
	if err != nil {
		e.mu.Unlock()
		return fmt.Errorf("start agent: %w", err)
	}
	e.agentSteps[agentID] = step.ID
	e.mu.Unlock()

	wfExec.State = ExecWaiting
	e.logger.Info("workflow.run-agent", "task_id", taskID, "step", step.ID, "role", step.Config.Role, "agent_id", agentID, "provider", provider)
	return e.tasks.SetWorkflow(taskID, wfExec)
}

// execParallel spawns every child of a `parallel` block concurrently, all
// against the shared task worktree. Children are headless run_agent steps
// (validated at definition load time); planning is read-only so contention
// on the worktree is benign. The parent step advances to its `next` only
// after every child has terminated; per-child completions are routed
// through AdvanceStep via the existing agentSteps mapping.
func (e *Engine) execParallel(taskID string, step *Step, wfExec *Execution, ctx TemplateContext) error {
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

	// Hold e.mu across StartAgent + registration — same race-close as execRunAgent.
	e.mu.Lock()
	agentID, err := e.agents.StartAgent(taskID, child.Config.Role, mode, model, provider, prompt, dir, child.Config.AllowedTools, child.Config.NeedsWorktree, oneShot)
	if err != nil {
		e.mu.Unlock()
		return fmt.Errorf("start agent: %w", err)
	}
	// agentSteps key uses the *child* step ID. StepByID recurses into
	// Parallel children so the lookup in lookupAgentStep / AdvanceStep
	// returns the right step config.
	e.agentSteps[agentID] = child.ID
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

	// All children terminated — collapse into a single parent step record
	// and advance via parent's Next.
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

	// Re-read task for latest state (children may have written sidecars).
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

// flipProvider returns the opposite provider.
func flipProvider(p string) string {
	if p == "codex" {
		return "claude"
	}
	return "codex"
}

// resolveProvider resolves the step-level provider string.
// "cross" flips the last agent step's provider; "" defers to manager default.
func resolveProvider(stepProv string, wfExec *Execution, defaultProv string) string {
	switch stepProv {
	case "cross":
		for i := range slices.Backward(wfExec.StepHistory) {
			if p := wfExec.StepHistory[i].Provider; p != "" {
				return flipProvider(p)
			}
		}
		return flipProvider(defaultProv)
	case "":
		return ""
	default:
		return stepProv
	}
}

// providerAvailable reports whether the CLI for a provider is on PATH.
// Indirected through a var so tests can short-circuit the lookup — engine
// unit tests run with mock agents and don't care whether the real CLI is
// installed on the runner; without the indirection a CI host without
// claude/codex on PATH causes the engine's fallback to strip the
// step-configured provider, breaking provider-aware assertions.
var providerAvailable = func(provider string) bool {
	_, err := exec.LookPath(provider)
	return err == nil
}

func (e *Engine) execWaitHuman(taskID string, step *Step, wfExec *Execution) error {
	if step.Config.Status != "" {
		if err := e.tasks.UpdateTaskStatus(taskID, step.Config.Status, step.Config.StatusReason); err != nil {
			return err
		}
	}

	wfExec.State = ExecWaiting
	e.logger.Info("workflow.wait-human", "task_id", taskID, "step", step.ID, "actions", step.Config.HumanActions)
	return e.tasks.SetWorkflow(taskID, wfExec)
}

func (e *Engine) execSetStatus(taskID string, step *Step) (StepOutput, error) {
	if err := e.tasks.UpdateTaskStatus(taskID, step.Config.Status, step.Config.StatusReason); err != nil {
		return StepOutput{}, err
	}

	e.logger.Info("workflow.set-status", "task_id", taskID, "step", step.ID, "status", step.Config.Status)
	return StepOutput{StepID: step.ID, Status: "completed"}, nil
}

func (e *Engine) execCondition(step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	// Condition is a no-op execution; transition resolution in the caller handles branching.
	_ = t
	_ = wfExec
	return StepOutput{StepID: step.ID, Status: "completed"}, nil
}

func (e *Engine) execShell(step *Step, ctx TemplateContext) (StepOutput, error) {
	command, err := RenderTemplate(step.Config.Command, ctx)
	if err != nil {
		return StepOutput{}, fmt.Errorf("render command: %w", err)
	}

	shellCtx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(shellCtx, "bash", "-c", command)
	if step.Config.Dir != "" {
		cmd.Dir = step.Config.Dir
	}

	// Expose task fields as env vars to avoid shell injection via template interpolation.
	ti := ctx.Task
	cmd.Env = append(cmd.Environ(),
		"SYBRA_TASK_ID="+ti.ID,
		"SYBRA_TASK_TITLE="+ti.Title,
		"SYBRA_TASK_STATUS="+ti.Status,
		"SYBRA_TASK_PROJECT="+ti.ProjectID,
		"SYBRA_TASK_BRANCH="+ti.Branch,
		fmt.Sprintf("SYBRA_TASK_PR=%d", ti.PRNumber),
	)

	output, runErr := cmd.CombinedOutput()
	status := "completed"
	if runErr != nil {
		status = "failed"
	}

	return StepOutput{
		StepID: step.ID,
		Status: status,
		Output: string(output),
	}, nil
}

// execEnsurePRClosesIssue verifies the task's PR closes its linked
// GitHub issue. When the closing reference is missing, it appends
// `Closes <issue-url>` to the PR body via the PRLinker and re-verifies.
// On verification failure the task is flipped to human-required so a
// human can fix the linkage manually.
//
// The step is a no-op when any of these are missing: task.Issue,
// task.PRNumber, task.ProjectID, engine.prLinker. It also skips when
// the issue lives in a different repo than the PR (cross-repo linking
// needs explicit support GitHub handles but this check does not).
func (e *Engine) execEnsurePRClosesIssue(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if e.prLinker == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no pr linker configured"}, nil
	}
	if t.Issue == "" || t.PRNumber == 0 || t.ProjectID == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: missing issue, pr, or project"}, nil
	}

	issueRepo, issueNum := parseIssueURL(t.Issue)
	if issueNum == 0 {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: unparseable issue url"}, nil
	}
	if issueRepo != t.ProjectID {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: cross-repo issue link"}, nil
	}

	issues, body, err := e.prLinker.GetClosingIssues(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Error("workflow.pr-close.fetch", "task_id", taskID, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "fetch failed: " + err.Error()}, nil
	}
	if slices.Contains(issues, issueNum) {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "already linked"}, nil
	}

	newBody := body
	if newBody != "" {
		newBody += "\n\n"
	}
	newBody += "Closes " + t.Issue
	if editErr := e.prLinker.EditBody(t.ProjectID, t.PRNumber, newBody); editErr != nil {
		e.logger.Error("workflow.pr-close.edit", "task_id", taskID, "err", editErr)
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", "PR does not close linked issue and auto-fix failed: "+editErr.Error()); statusErr != nil {
			e.logger.Error("workflow.pr-close.status", "task_id", taskID, "err", statusErr)
		}
		return StepOutput{StepID: step.ID, Status: "failed", Output: "edit failed: " + editErr.Error()}, nil
	}

	// Verify with retry — GitHub updates closingIssuesReferences
	// asynchronously after a body edit, so the first fetch can miss
	// refs that populate seconds later. If every retry still misses,
	// trust the body: we just wrote "Closes <url>" into it with a
	// known-good format, so the link will resolve once GitHub catches
	// up. Only edit failures (above) flip to human-required.
	var verifyErr error
	for attempt := 0; attempt <= len(prVerifyBackoffs); attempt++ {
		if attempt > 0 {
			prVerifySleep(prVerifyBackoffs[attempt-1])
		}
		var verified []int
		verified, _, verifyErr = e.prLinker.GetClosingIssues(t.ProjectID, t.PRNumber)
		if verifyErr == nil && slices.Contains(verified, issueNum) {
			e.logger.Info("workflow.pr-close.linked", "task_id", taskID, "pr", t.PRNumber, "issue", issueNum, "attempt", attempt)
			return StepOutput{StepID: step.ID, Status: "completed", Output: fmt.Sprintf("linked issue #%d", issueNum)}, nil
		}
	}

	e.logger.Warn("workflow.pr-close.verify-lag", "task_id", taskID, "pr", t.PRNumber, "issue", issueNum, "err", verifyErr)
	msg := fmt.Sprintf("edited body to close #%d; verification lagged — trusting body contents", issueNum)
	if verifyErr != nil {
		msg = fmt.Sprintf("edited body to close #%d; last verify err: %s — trusting body contents", issueNum, verifyErr.Error())
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
}

// execVerifyCommits checks that the task's branch has at least one commit
// ahead of origin/main. This is a non-LLM mechanical gate that runs before
// the eval agent to detect incomplete work without giving eval git access.
//
// Skip conditions (no-op, returns "completed"):
//   - No WorktreeGetter configured
//   - No worktree found for the task
//
// When the branch has no commits ahead of origin/main, OR when the git
// command fails (broken worktree, missing bare clone, unresolvable HEAD),
// the task is flipped to human-required and the step returns "completed" so
// the workflow can route to end via a task.status transition condition.
// Treating git failures as a hard gate prevents the workflow from wasting
// `code_review`/`fix_review`/`create_pr` cycles on a worktree the agent
// cannot operate in.
// execRequireSidecar verifies that the configured sidecar was actually
// written for the task. When empty, flips the task to human-required
// with a descriptive reason instead of silently advancing the workflow.
// Catches the codex-sandbox-blocked class of failure where the agent
// exits cleanly without producing its expected output file.
func (e *Engine) execRequireSidecar(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	var content, label string
	switch sk := step.Config.Sidecar; {
	case sk == "plan_critique":
		content = t.PlanCritique
		label = "plan critique"
	case sk == "code_review":
		content = t.CodeReview
		label = "code review"
	case sk == "plan":
		content = t.Plan
		label = "plan"
	case strings.HasPrefix(sk, "plan_draft."):
		name := strings.TrimPrefix(sk, "plan_draft.")
		content = t.PlanDrafts[name]
		label = "plan draft " + name
	case sk == "":
		return StepOutput{}, fmt.Errorf("require_sidecar: config.sidecar is required")
	default:
		return StepOutput{}, fmt.Errorf("require_sidecar: unknown sidecar %q (want plan|plan_critique|code_review|plan_draft.<name>)", sk)
	}
	if strings.TrimSpace(content) == "" {
		reason := label + " missing — upstream agent step completed without writing its sidecar"
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.require-sidecar.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.require-sidecar.missing", "task_id", taskID, "sidecar", step.Config.Sidecar)
		return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
	}
	return StepOutput{StepID: step.ID, Status: "completed", Output: label + " present"}, nil
}

// resolveOriginBase returns the remote ref to use as the base for commit
// range comparisons. It checks for origin/HEAD (set when the remote HEAD
// symbolic ref is configured), then falls back to probing master and main.
// Returns "origin/main" if nothing resolves.
func resolveOriginBase(ctx context.Context, wtPath string) string {
	for _, candidate := range []string{"origin/HEAD", "origin/master", "origin/main"} {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", candidate)
		cmd.Dir = wtPath
		if cmd.Run() == nil {
			return candidate
		}
	}
	return "origin/main"
}

func (e *Engine) execVerifyCommits(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if e.worktrees == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree getter configured"}, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree for task"}, nil
	}

	output, err := e.gitLogAheadOfBase(wtPath)
	if err != nil && !errors.Is(e.ctx.Err(), context.Canceled) && !errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
		verifyCommitsRetrySleep(verifyCommitsRetryBackoff)
		output, err = e.gitLogAheadOfBase(wtPath)
	}
	if err != nil {
		// Context cancellation indicates engine shutdown, not a worktree
		// problem — leave task status alone so it resumes on next boot.
		if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
			e.logger.Warn("workflow.verify-commits.canceled", "task_id", taskID, "err", err)
			return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: context canceled"}, nil
		}
		diagnosis := diagnoseWorktreeState(e.ctx, wtPath)
		e.logger.Warn("workflow.verify-commits.git-error", "task_id", taskID, "worktree", wtPath, "err", err, "diagnosis", diagnosis)
		reason := "worktree git error: " + err.Error()
		if diagnosis != "" {
			reason += " (" + diagnosis + ")"
		}
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "git error: flipped to human-required"}, nil
	}

	if strings.TrimSpace(string(output)) == "" {
		reason := "no commits pushed to branch"
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.verify-commits.status", "task_id", taskID, "err", statusErr)
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "no commits: flipped to human-required"}, nil
	}

	return StepOutput{StepID: step.ID, Status: "completed", Output: "commits verified"}, nil
}

func (e *Engine) gitLogAheadOfBase(wtPath string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	baseRef := resolveOriginBase(ctx, wtPath)
	cmd := exec.CommandContext(ctx, "git", "log", baseRef+"..HEAD", "--oneline")
	cmd.Dir = wtPath
	return cmd.Output()
}

// diagnoseWorktreeState produces a short human-readable hint about why a
// worktree's `git log` failed. Returns "" when nothing concrete is found.
// Used to enrich the human-required status_reason so triage doesn't have
// to grep agent logs to figure out whether the worktree was missing,
// dirty, or just had a stale lock.
func diagnoseWorktreeState(parentCtx context.Context, wtPath string) string {
	ctx, cancel := context.WithTimeout(parentCtx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		first = strings.TrimSpace(first)
		if first == "" {
			return "git status failed: " + err.Error()
		}
		return "git status: " + first
	}
	if dirty := strings.TrimSpace(string(out)); dirty != "" {
		entries := strings.Count(dirty, "\n") + 1
		return fmt.Sprintf("dirty worktree (%d uncommitted entries)", entries)
	}
	return "clean tree, no commits ahead"
}

var prURLRe = regexp.MustCompile(`github\.com/[^/\s]+/[^/\s]+/pull/(\d+)`)
var prShortRe = regexp.MustCompile(`\b[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#(\d+)`)

// execLinkPRAndReview is a non-LLM mechanical step that tries to recover the
// PR number from three sources and flip the task to in-review:
//
//  1. task.pr_number already set → set in-review, skip eval
//  2. regex match on agent result text in step history → link + in-review
//  3. gh pr list --head <branch> → single result → link + in-review
//
// When no PR is found the step returns without touching task status, allowing
// the workflow to fall through to the LLM eval step.
func (e *Engine) execLinkPRAndReview(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	setInReview := func(prNumber int, source string) (StepOutput, error) {
		if err := e.tasks.UpdateTaskPR(taskID, prNumber); err != nil {
			return StepOutput{}, fmt.Errorf("link pr: %w", err)
		}
		if err := e.tasks.UpdateTaskStatus(taskID, "in-review", ""); err != nil {
			return StepOutput{}, fmt.Errorf("set in-review: %w", err)
		}
		msg := fmt.Sprintf("pr #%d found via %s → in-review", prNumber, source)
		e.logger.Info("workflow.link-pr.linked", "task_id", taskID, "pr", prNumber, "source", source)
		return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
	}

	// Path 1: PR already linked on task.
	if t.PRNumber > 0 {
		return setInReview(t.PRNumber, "task.pr_number")
	}

	// Path 2: Scan step history for a GitHub PR URL or owner/repo#N in agent output.
	for i := range slices.Backward(wfExec.StepHistory) {
		rec := wfExec.StepHistory[i]
		if rec.Status != "completed" || rec.Output == "" {
			continue
		}
		for _, re := range []*regexp.Regexp{prURLRe, prShortRe} {
			if m := re.FindStringSubmatch(rec.Output); len(m) > 1 {
				n, err := strconv.Atoi(m[1])
				if err == nil && n > 0 {
					return setInReview(n, "agent result")
				}
			}
		}
	}

	// Path 3: Query GitHub when branch is known.
	// Use bash -c with env vars to keep project/branch out of arg list (gosec G204).
	if t.ProjectID != "" && t.Branch != "" {
		ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-c",
			"gh pr list --repo \"$_REPO\" --head \"$_BRANCH\" --json number --limit 2")
		cmd.Env = append(cmd.Environ(), "_REPO="+t.ProjectID, "_BRANCH="+t.Branch)
		out, err := cmd.Output()
		if err != nil {
			e.logger.Warn("workflow.link-pr.gh-list", "task_id", taskID, "err", err)
		} else {
			var prs []struct {
				Number int `json:"number"`
			}
			jsonErr := json.Unmarshal(out, &prs)
			if jsonErr == nil && len(prs) == 1 {
				return setInReview(prs[0].Number, "gh pr list")
			}
			if jsonErr != nil {
				// gh --json returned malformed output. Don't mask the upstream
				// failure as "no pr found" — log so operators can diagnose.
				e.logger.Warn("workflow.link-pr.gh-list.parse", "task_id", taskID, "err", jsonErr, "raw", truncate(string(out), 200))
			}
		}
	}

	e.logger.Info("workflow.link-pr.no-pr", "task_id", taskID)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "no pr found: falling through to eval"}, nil
}

// execEvaluate is a non-LLM mechanical step that decides the terminal status
// after link_pr_and_review has exhausted its PR-discovery paths. It walks step
// history backwards for the most recent run_agent record (the impl/fix step)
// and flips the task to human-required with a bounded reason string.
//
// Before giving up, it does a final gh pr list check — guarding against the
// race where the agent created a PR after link_pr_and_review already ran.
func (e *Engine) execEvaluate(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	// Final GitHub PR check — catches PRs created after link_pr_and_review ran.
	if t.ProjectID != "" && t.Branch != "" {
		ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-c",
			"gh pr list --repo \"$_REPO\" --head \"$_BRANCH\" --json number --limit 2")
		cmd.Env = append(cmd.Environ(), "_REPO="+t.ProjectID, "_BRANCH="+t.Branch)
		if out, err := cmd.Output(); err == nil {
			var prs []struct {
				Number int `json:"number"`
			}
			jsonErr := json.Unmarshal(out, &prs)
			if jsonErr == nil && len(prs) == 1 {
				prNum := prs[0].Number
				if linkErr := e.tasks.UpdateTaskPR(taskID, prNum); linkErr != nil {
					return StepOutput{}, fmt.Errorf("evaluate: link pr: %w", linkErr)
				}
				if linkErr := e.tasks.UpdateTaskStatus(taskID, "in-review", ""); linkErr != nil {
					return StepOutput{}, fmt.Errorf("evaluate: set in-review: %w", linkErr)
				}
				msg := fmt.Sprintf("pr #%d found via late gh pr list → in-review", prNum)
				e.logger.Info("workflow.evaluate.late-pr-found", "task_id", taskID, "pr", prNum)
				return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
			}
			if jsonErr != nil {
				e.logger.Warn("workflow.evaluate.gh-list.parse", "task_id", taskID, "err", jsonErr, "raw", truncate(string(out), 200))
			}
		}
	}

	var last *StepRecord
	for i := range slices.Backward(wfExec.StepHistory) {
		if wfExec.StepHistory[i].AgentID != "" {
			last = &wfExec.StepHistory[i]
			break
		}
	}

	reason := "no agent result to evaluate"
	if last != nil {
		if last.Status == "failed" {
			reason = truncate(strings.TrimSpace(last.Output), 200)
			if reason == "" {
				reason = "agent failed with no output"
			}
		} else {
			reason = "commits pushed but no PR created"
		}
	}

	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, fmt.Errorf("evaluate: set human-required: %w", err)
	}
	e.logger.Info("workflow.evaluate.human-required", "task_id", taskID, "reason", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}

// parseIssueURL extracts owner/repo and issue number from a GitHub
// issue URL like https://github.com/owner/repo/issues/123. Returns
// ("", 0) if the URL doesn't match. Duplicated from internal/github
// to keep the workflow package dependency-free.
func parseIssueURL(rawURL string) (repo string, number int) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(rawURL, prefix) {
		return "", 0
	}
	parts := strings.Split(strings.TrimPrefix(rawURL, prefix), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return "", 0
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n == 0 {
		return "", 0
	}
	return parts[0] + "/" + parts[1], n
}
