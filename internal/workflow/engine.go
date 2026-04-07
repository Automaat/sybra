package workflow

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// TaskInfo is the subset of task data the engine needs.
type TaskInfo struct {
	ID        string
	Title     string
	Status    string
	Tags      []string
	AgentMode string
	ProjectID string
	PRNumber  int
	Branch    string
	Body      string
	Workflow  *Execution
}

// TaskProvider reads and updates tasks.
type TaskProvider interface {
	GetTask(id string) (TaskInfo, error)
	UpdateTaskStatus(id, status, reason string) error
	SetWorkflow(id string, wf *Execution) error
}

// AgentLauncher starts agents and queries running state.
type AgentLauncher interface {
	StartAgent(taskID, role, mode, model, prompt string, allowedTools []string) (agentID string, err error)
	HasRunningAgent(taskID string) bool
	StopAgentsForTask(taskID string, role string)
	SendPrompt(agentID, message string) error
}

// Engine executes workflow definitions against tasks.
type Engine struct {
	store    *Store
	tasks    TaskProvider
	agents   AgentLauncher
	logger   *slog.Logger
	mu       sync.Mutex
	inflight map[string]struct{} // taskID → step in flight (prevent double-advance)
}

// NewEngine creates a workflow engine.
func NewEngine(store *Store, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger) *Engine {
	return &Engine{
		store:    store,
		tasks:    tasks,
		agents:   agents,
		logger:   logger,
		inflight: make(map[string]struct{}),
	}
}

// Defs returns the workflow definition store.
func (e *Engine) Defs() *Store { return e.store }

// StartWorkflow assigns a workflow to a task and executes the first step.
func (e *Engine) StartWorkflow(taskID, workflowID string) error {
	def, err := e.store.Get(workflowID)
	if err != nil {
		return fmt.Errorf("get workflow %s: %w", workflowID, err)
	}

	first := def.FirstStep()
	if first == nil {
		return fmt.Errorf("workflow %s has no steps", workflowID)
	}

	wfExec := &Execution{
		WorkflowID:  workflowID,
		CurrentStep: first.ID,
		State:       ExecRunning,
		Variables:   make(map[string]string),
		StartedAt:   time.Now().UTC(),
	}

	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return fmt.Errorf("set workflow on task: %w", err)
	}

	e.logger.Info("workflow.start", "task_id", taskID, "workflow", workflowID, "step", first.ID)
	return e.executeStep(taskID, &def, first, wfExec)
}

// MatchWorkflow finds the best workflow for a task based on trigger conditions.
func (e *Engine) MatchWorkflow(t TaskInfo, event string) *Definition {
	defs, err := e.store.List()
	if err != nil {
		e.logger.Error("workflow.match.list", "err", err)
		return nil
	}

	fields := taskFields(t)
	for i := range defs {
		if defs[i].Trigger.On != event {
			continue
		}
		if EvalConditions(defs[i].Trigger.Conditions, fields) {
			return &defs[i]
		}
	}
	return nil
}

// AdvanceStep is called when a step completes. It records the result,
// evaluates transitions, and executes the next step.
func (e *Engine) AdvanceStep(taskID string, output StepOutput) error {
	e.mu.Lock()
	if _, ok := e.inflight[taskID]; ok {
		e.mu.Unlock()
		e.logger.Debug("workflow.advance.skip", "task_id", taskID, "reason", "already_advancing")
		return nil
	}
	e.inflight[taskID] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.inflight, taskID)
		e.mu.Unlock()
	}()

	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if t.Workflow == nil {
		return fmt.Errorf("task %s has no active workflow", taskID)
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return err
	}

	wfExec := t.Workflow

	// Record step completion.
	now := time.Now().UTC()
	wfExec.StepHistory = append(wfExec.StepHistory, StepRecord{
		StepID:    output.StepID,
		Status:    output.Status,
		Output:    truncate(output.Output, 4000),
		AgentID:   output.AgentID,
		StartedAt: now, // approximate
		EndedAt:   now,
	})

	// Store output in variables for template access.
	if output.Output != "" {
		wfExec.SetVar("step."+output.StepID+".output", truncate(output.Output, 2000))
	}

	currentStep := def.StepByID(output.StepID)
	if currentStep == nil {
		return fmt.Errorf("step %s not found in workflow %s", output.StepID, def.ID)
	}

	// Re-read task for latest state (triage agent may have changed tags/status).
	t, err = e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	// Restore execution (re-read may have stale copy).
	t.Workflow = wfExec

	fields := taskFields(t)
	for k, v := range wfExec.Variables {
		fields["vars."+k] = v
	}

	nextID, tErr := ResolveTransition(currentStep.Next, fields)
	if tErr != nil {
		e.logger.Error("workflow.transition.failed", "task_id", taskID, "step", output.StepID, "err", tErr)
		wfExec.State = ExecFailed
		return e.tasks.SetWorkflow(taskID, wfExec)
	}

	if nextID == "" {
		// End of workflow.
		wfExec.State = ExecCompleted
		completedAt := now
		wfExec.CompletedAt = &completedAt
		wfExec.CurrentStep = ""
		e.logger.Info("workflow.completed", "task_id", taskID, "workflow", def.ID)
		return e.tasks.SetWorkflow(taskID, wfExec)
	}

	nextStep := def.StepByID(nextID)
	if nextStep == nil {
		return fmt.Errorf("next step %s not found in workflow %s", nextID, def.ID)
	}

	wfExec.CurrentStep = nextStep.ID
	wfExec.State = ExecRunning
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}

	e.logger.Info("workflow.advance", "task_id", taskID, "from", output.StepID, "to", nextStep.ID)
	return e.executeStep(taskID, &def, nextStep, wfExec)
}

// HandleHumanAction processes approve/reject/input from the UI.
func (e *Engine) HandleHumanAction(taskID, action string, data map[string]string) error {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if t.Workflow == nil || t.Workflow.State != ExecWaiting {
		return fmt.Errorf("task %s is not waiting for human action", taskID)
	}

	wfExec := t.Workflow
	wfExec.SetVar("human_action", action)
	for k, v := range data {
		wfExec.SetVar("human."+k, v)
	}

	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}

	return e.AdvanceStep(taskID, StepOutput{
		StepID: wfExec.CurrentStep,
		Status: "completed",
		Output: action,
	})
}

// HandleAgentComplete is called when an agent finishes. It maps the agent
// back to the workflow step and advances.
func (e *Engine) HandleAgentComplete(taskID, agentID, result string) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Error("workflow.agent-complete.get", "task_id", taskID, "err", err)
		return
	}
	if t.Workflow == nil {
		e.logger.Debug("workflow.agent-complete.no-workflow", "task_id", taskID)
		return
	}

	if err := e.AdvanceStep(taskID, StepOutput{
		StepID:  t.Workflow.CurrentStep,
		Status:  "completed",
		Output:  result,
		AgentID: agentID,
	}); err != nil {
		e.logger.Error("workflow.agent-complete.advance", "task_id", taskID, "err", err)
	}
}

// ResumeStalled finds tasks with running/waiting workflows where no agent
// is active, and attempts to re-execute the current step.
func (e *Engine) ResumeStalled() {
	// This would need to list all tasks and check their workflow state.
	// For now, this is a placeholder — the orchestrator loop will call it.
	// Implementation deferred to Phase 2 when we wire the engine into the app.
}

func (e *Engine) executeStep(taskID string, def *Definition, step *Step, wfExec *Execution) error {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}

	ctx := TemplateContext{
		Task:    t,
		Step:    *step,
		Prev:    wfExec.LastRecord(),
		Vars:    wfExec.Variables,
		Project: nil,
	}

	switch step.Type {
	case StepRunAgent:
		return e.execRunAgent(taskID, step, wfExec, ctx)
	case StepWaitHuman:
		return e.execWaitHuman(taskID, step, wfExec)
	case StepSetStatus:
		return e.execSetStatus(taskID, step, wfExec)
	case StepCondition:
		return e.execCondition(taskID, def, step, wfExec, t)
	case StepShell:
		return e.execShell(taskID, step, wfExec, ctx)
	case StepParallel:
		return e.execParallel(taskID, step, wfExec)
	default:
		return fmt.Errorf("unknown step type %q", step.Type)
	}
}

func (e *Engine) execRunAgent(taskID string, step *Step, wfExec *Execution, ctx TemplateContext) error {
	prompt, err := RenderTemplate(step.Config.Prompt, ctx)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
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

	agentID, err := e.agents.StartAgent(taskID, step.Config.Role, mode, model, prompt, step.Config.AllowedTools)
	if err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	wfExec.State = ExecWaiting
	wfExec.StepHistory = append(wfExec.StepHistory, StepRecord{
		StepID:    step.ID,
		Status:    "running",
		AgentID:   agentID,
		StartedAt: time.Now().UTC(),
	})

	e.logger.Info("workflow.run-agent", "task_id", taskID, "step", step.ID, "role", step.Config.Role, "agent_id", agentID)
	return e.tasks.SetWorkflow(taskID, wfExec)
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

func (e *Engine) execSetStatus(taskID string, step *Step, wfExec *Execution) error {
	if err := e.tasks.UpdateTaskStatus(taskID, step.Config.Status, step.Config.StatusReason); err != nil {
		return err
	}

	e.logger.Info("workflow.set-status", "task_id", taskID, "step", step.ID, "status", step.Config.Status)

	return e.AdvanceStep(taskID, StepOutput{
		StepID: step.ID,
		Status: "completed",
	})
}

func (e *Engine) execCondition(taskID string, def *Definition, step *Step, wfExec *Execution, t TaskInfo) error {
	fields := taskFields(t)
	for k, v := range wfExec.Variables {
		fields["vars."+k] = v
	}

	nextID, err := ResolveTransition(step.Next, fields)
	if err != nil {
		return err
	}

	wfExec.StepHistory = append(wfExec.StepHistory, StepRecord{
		StepID:    step.ID,
		Status:    "completed",
		StartedAt: time.Now().UTC(),
		EndedAt:   time.Now().UTC(),
	})

	if nextID == "" {
		wfExec.State = ExecCompleted
		now := time.Now().UTC()
		wfExec.CompletedAt = &now
		wfExec.CurrentStep = ""
		return e.tasks.SetWorkflow(taskID, wfExec)
	}

	nextStep := def.StepByID(nextID)
	if nextStep == nil {
		return fmt.Errorf("step %s not found", nextID)
	}

	wfExec.CurrentStep = nextStep.ID
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}

	return e.executeStep(taskID, def, nextStep, wfExec)
}

func (e *Engine) execShell(taskID string, step *Step, wfExec *Execution, ctx TemplateContext) error {
	command, err := RenderTemplate(step.Config.Command, ctx)
	if err != nil {
		return fmt.Errorf("render command: %w", err)
	}

	args := []string{"-c", command}
	cmd := exec.Command("bash", args...)
	output, runErr := cmd.CombinedOutput()
	status := "completed"
	if runErr != nil {
		status = "failed"
	}

	return e.AdvanceStep(taskID, StepOutput{
		StepID: step.ID,
		Status: status,
		Output: string(output),
	})
}

func (e *Engine) execParallel(_ string, _ *Step, _ *Execution) error {
	// Parallel execution is deferred to Phase 2.
	return fmt.Errorf("parallel steps not yet implemented")
}

func taskFields(t TaskInfo) map[string]string {
	fields := map[string]string{
		"task.id":         t.ID,
		"task.title":      t.Title,
		"task.status":     t.Status,
		"task.tags":       strings.Join(t.Tags, ","),
		"task.agent_mode": t.AgentMode,
		"task.project_id": t.ProjectID,
		"task.branch":     t.Branch,
	}
	if t.PRNumber > 0 {
		fields["task.pr_number"] = fmt.Sprintf("%d", t.PRNumber)
	}
	return fields
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n... (truncated)"
}
