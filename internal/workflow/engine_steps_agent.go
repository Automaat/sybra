package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/skillinvoke"
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
	if step == nil || step.Type != StepRunAgent {
		return
	}
	for _, cfg := range step.Config.sidecarImports() {
		e.importOneSidecar(taskID, stepID, step, info, cfg)
	}
}

func (c StepConfig) sidecarImports() []ImportSidecar {
	var out []ImportSidecar
	if c.ImportSidecar != nil {
		out = append(out, *c.ImportSidecar)
	}
	out = append(out, c.ImportSidecars...)
	return out
}

func (e *Engine) importOneSidecar(taskID, stepID string, step *Step, info TaskInfo, cfg ImportSidecar) {
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
		if cfg.Required {
			e.failRequiredImport(taskID, stepID, cfg.Kind, "missing")
		}
		return
	}
	if cfg.Required && strings.TrimSpace(string(content)) == "" {
		e.failRequiredImport(taskID, stepID, cfg.Kind, "empty")
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

	// Capture a plan artifact for plan-kind sidecars so the raw markdown is
	// available for later agent re-reading alongside its provenance metadata.
	if cfg.Kind == "plan" && e.recorder != nil {
		if recErr := e.recorder.PutPlanSnapshot(taskID, step.Config.Role, stepID, path, string(content)); recErr != nil {
			e.logger.Warn("artifact.record.failed", "kind", "plan", "task_id", taskID, "step", stepID, "err", recErr)
		}
	}
	if cfg.Kind == "plan_contract" && e.recorder != nil {
		name := "plan-contract.json"
		if stepID != "" {
			name = "plan-contract-" + stepID + ".json"
		}
		if recErr := e.recorder.PutGeneric(taskID, name, stepID, string(content)); recErr != nil {
			e.logger.Warn("artifact.record.failed", "kind", "plan_contract", "task_id", taskID, "step", stepID, "err", recErr)
		}
	}
}

func (e *Engine) failRequiredImport(taskID, stepID, kind, state string) {
	reason := fmt.Sprintf("required %s sidecar %s", strings.ReplaceAll(kind, "_", " "), state)
	if stepID != "" {
		reason += " after step " + stepID
	}
	if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
		e.logger.Error("workflow.import-sidecar.required.status", "task_id", taskID, "step", stepID, "kind", kind, "err", statusErr)
	}
}

func (e *Engine) execRunAgent(taskID string, step *Step, wfExec *Execution, ctx TemplateContext) error {
	prepareTestVerdictAttemptVars(wfExec, step.ID, ctx.Task.Body)

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

	provider, model, assignment, err := e.resolveAgentVariant(ctx.Task, step, wfExec, model, "workflow.cross-provider.fallback")
	if err != nil {
		return err
	}

	prompt, err := e.renderAssignedPrompt(taskID, step, ctx, assignment, "workflow.consume-steer")
	if err != nil {
		return err
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

	dir := wfExec.Variables[WorkflowVarDir]

	// Stop stale agents left over from earlier workflow steps (e.g. an
	// interactive plan agent with reuse_agent that outlived plan approval).
	// Empty role = stop all roles for this task.
	e.agents.StopAgentsForTask(taskID, "")
	// Drop the stopped agents' step mappings so a late/double completion from a
	// superseded agent (e.g. a stopped test-runner during a run_test retry) is
	// treated as untracked and dropped rather than counted against the step's
	// retry budget. The agent spawned just below becomes the only tracked one.
	e.clearAgentStepsForTask(taskID)

	// Interactive agents that aren't meant to persist across turns (no
	// reuse_agent, no wait_for_status) must signal completion via process
	// exit. OneShot tells the runner to close stdin after the first result
	// event so claude exits and onComplete fires, unblocking the next step
	// (e.g. evaluate). Without this, the workflow stalls on implement forever.
	oneShot := mode == "interactive" && !step.Config.ReuseAgent && step.Config.WaitForStatus == ""
	agentID, startedDir, baselineRef, err := e.agents.StartAgent(taskID, step.Config.Role, mode, model, provider, prompt, dir, step.Config.AllowedTools, step.Config.NeedsWorktree, oneShot, step.Config.OutputSchema, assignment)
	if err != nil {
		// Another dispatcher already holds the per-task dispatch claim (e.g. the
		// recovery loop won the race for this task). That agent will run and its
		// completion drives the workflow forward — so wait rather than failing
		// the step (which would otherwise route into verify_commits /
		// human-required on a task that has live work in flight).
		if errors.Is(err, ErrDispatchInFlight) {
			wfExec.State = ExecWaiting
			e.logger.Info("workflow.run-agent.dispatch-in-flight", "task_id", taskID, "step", step.ID)
			return e.tasks.SetWorkflow(taskID, wfExec)
		}

		// Testing concurrency cap saturated — park and let ResumeStalled retry
		// when a test-runner slot frees, same as dispatch-in-flight.
		if errors.Is(err, ErrTestRunnerBusy) {
			wfExec.State = ExecWaiting
			e.logger.Info("workflow.run-agent.test-runner-busy", "task_id", taskID, "step", step.ID)
			return e.tasks.SetWorkflow(taskID, wfExec)
		}
		return fmt.Errorf("start agent: %w", err)
	}
	if startedDir != "" && (step.Config.NeedsWorktree || dir != "") {
		wfExec.SetVar(WorkflowVarDir, startedDir)
	}
	if baselineRef != "" {
		wfExec.SetVar(tamperBaselineVar(step.ID), baselineRef)
	}

	// Track which task+step this agent was spawned for so HandleAgentComplete
	// can detect stale completions (e.g. duplicate agent from a ResumeStalled
	// race) rather than blindly crediting the current step.
	e.mu.Lock()
	e.agentSteps[agentID] = agentEntry{taskID: taskID, stepID: step.ID}
	e.mu.Unlock()

	wfExec.State = ExecWaiting
	e.logger.Info("workflow.run-agent", "task_id", taskID, "step", step.ID, "role", step.Config.Role, "agent_id", agentID, "provider", provider)
	return e.tasks.SetWorkflow(taskID, wfExec)
}

func (e *Engine) selectABVariant(ctx abtest.SelectionContext) (AgentAssignment, bool, error) {
	providerAllowed := func(provider string) bool {
		return providerAvailable(provider) && !e.agents.ProviderRateLimited(provider)
	}
	a, ok, err := abtest.SelectEligibleForContext(e.abTesting, ctx, providerAllowed)
	if err != nil || !ok {
		return AgentAssignment{}, ok, err
	}
	return AgentAssignment{
		ExperimentID:    a.ExperimentID,
		Kind:            a.Kind,
		VariantID:       a.VariantID,
		Provider:        a.Provider,
		Model:           a.Model,
		AssignmentUnit:  a.AssignmentUnit,
		AssignmentKey:   a.AssignmentKey,
		ReasoningEffort: a.ReasoningEffort,
		PromptTransform: workflowPromptTransform(a.PromptTransform),
		SkillAliases:    cloneWorkflowSkillAliases(a.SkillAliases),
	}, true, nil
}

func (e *Engine) renderAssignedPrompt(taskID string, step *Step, ctx TemplateContext, assignment AgentAssignment, steerLog string) (string, error) {
	templateText := applyPromptTransform(step.Config.Prompt, assignment.PromptTransform)
	prompt, err := RenderTemplate(templateText, ctx)
	if err != nil {
		return "", fmt.Errorf("render prompt: %w", err)
	}
	if steered, sErr := e.tasks.ConsumeSupervisorSteer(taskID, prompt); sErr != nil {
		e.logger.Warn(steerLog, "task_id", taskID, "step", step.ID, "err", sErr)
	} else {
		prompt = steered
	}
	return skillinvoke.ApplyAliases(prompt, assignment.SkillAliases), nil
}

func applyPromptTransform(prompt string, transform *PromptTransform) string {
	if transform == nil {
		return prompt
	}
	switch strings.TrimSpace(transform.Op) {
	case "replace", "template":
		return transform.Text
	case "prepend":
		return transform.Text + prompt
	case "append":
		return prompt + transform.Text
	default:
		return prompt
	}
}

func workflowPromptTransform(in *abtest.PromptTransform) *PromptTransform {
	if in == nil {
		return nil
	}
	return &PromptTransform{Op: in.Op, Text: in.Text}
}

func cloneWorkflowSkillAliases(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func (e *Engine) resolveAgentVariant(t TaskInfo, step *Step, wfExec *Execution, model, fallbackLog string) (provider, resolvedModel string, assignment AgentAssignment, err error) {
	defaultModel := model
	provider = resolveProvider(step.Config.Provider, wfExec, e.agents.DefaultProvider(), t)
	resolvedModel = model
	if step.Config.Provider == "" || step.Config.Provider == "ab" {
		selected, ok, err := e.selectABVariant(abtest.SelectionContext{
			TaskID:     t.ID,
			WorkflowID: wfExec.WorkflowID,
			Role:       step.Config.Role,
			StepID:     step.ID,
			Prompt:     step.Config.Prompt,
		})
		if err != nil {
			return "", "", AgentAssignment{}, fmt.Errorf("select ab variant: %w", err)
		}
		if ok {
			provider = selected.Provider
			resolvedModel = selected.Model
			assignment = selected
			if selected.ReasoningEffort != "" {
				wfExec.SetVar("ab."+step.ID+".reasoning_effort", selected.ReasoningEffort)
			}
		}
	}
	if provider != "" && !providerAvailable(provider) {
		e.logger.Warn(fallbackLog, "wanted", provider, "reason", "CLI not found")
		return "", defaultModel, AgentAssignment{}, nil
	}
	return provider, resolvedModel, assignment, nil
}

// resolveProvider resolves the step-level provider string.
// "cross" flips the most relevant code-producing provider; "" defers to the
// manager default.
func resolveProvider(stepProv string, wfExec *Execution, _ string, t TaskInfo) string {
	switch stepProv {
	case "cross":
		if p := lastWorkflowProvider(wfExec); p != "" {
			return crossProvider(p)
		}
		if p := lastCodeAuthorProvider(t.AgentRuns); p != "" {
			return crossProvider(p)
		}
		if p := normalizeExplicitWorkflowProvider(t.HandoffSourceProvider); p != "" {
			return crossProvider(p)
		}
		return ""
	case "":
		return ""
	default:
		return stepProv
	}
}

func lastWorkflowProvider(wfExec *Execution) string {
	if wfExec == nil {
		return ""
	}
	for i := range slices.Backward(wfExec.StepHistory) {
		if p := normalizeExplicitWorkflowProvider(wfExec.StepHistory[i].Provider); p != "" {
			return p
		}
	}
	return ""
}

func lastCodeAuthorProvider(runs []AgentRunInfo) string {
	for i := range slices.Backward(runs) {
		if !isCodeAuthorRole(runs[i].Role) {
			continue
		}
		if p := normalizeExplicitWorkflowProvider(runs[i].Provider); p != "" {
			return p
		}
	}
	return ""
}

func isCodeAuthorRole(role string) bool {
	switch role {
	case "", "implementation", "fix-review", "pr-fix":
		return true
	default:
		return false
	}
}

func crossProvider(provider string) string {
	switch normalizeWorkflowProvider(provider) {
	case "codex", "copilot":
		return "claude"
	default:
		return "codex"
	}
}

func normalizeWorkflowProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "claude":
		return "claude"
	case "codex":
		return "codex"
	case "copilot":
		return "copilot"
	default:
		return ""
	}
}

func normalizeExplicitWorkflowProvider(provider string) string {
	if strings.TrimSpace(provider) == "" {
		return ""
	}
	return normalizeWorkflowProvider(provider)
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
