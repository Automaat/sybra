package workflow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
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
	agentID, err := e.agents.StartAgent(taskID, step.Config.Role, mode, model, provider, prompt, dir, step.Config.AllowedTools, step.Config.NeedsWorktree, oneShot)
	if err != nil {
		return fmt.Errorf("start agent: %w", err)
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
