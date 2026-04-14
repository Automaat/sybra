package workflow

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/logging"
)

const (
	maxSyncSteps   = 100 // depth limit for synchronous step chains
	maxStepHistory = 50  // max step records kept per execution
	shellTimeout   = 30 * time.Second
)

// prVerifyBackoffs controls the retry schedule after editing a PR
// body to add a closing reference. GitHub populates
// closingIssuesReferences asynchronously, so a verify fetch right
// after gh pr edit commonly reads stale data; back off and retry.
// Indirected for tests — test init swaps in zeros to skip real waits.
var (
	prVerifyBackoffs = []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second}
	prVerifySleep    = time.Sleep
)

// TaskInfo is the subset of task data the engine needs.
type TaskInfo struct {
	ID           string
	Title        string
	Status       string
	Tags         []string
	AgentMode    string
	ProjectID    string
	PRNumber     int
	Branch       string
	Body         string
	Plan         string
	PlanCritique string
	Issue        string
	Workflow     *Execution
}

// TaskProvider reads and updates tasks.
type TaskProvider interface {
	GetTask(id string) (TaskInfo, error)
	ListTasks() ([]TaskInfo, error)
	UpdateTaskStatus(id, status, reason string) error
	UpdateTaskPR(id string, prNumber int) error
	SetWorkflow(id string, wf *Execution) error
}

// WorktreeGetter resolves the filesystem path of a task's git worktree.
// Returns (path, true) when a worktree exists for the task; ("", false) when
// none is found. Implementations may stat the path to confirm existence.
// Engine operates with a nil WorktreeGetter — verify_commits becomes a no-op.
type WorktreeGetter interface {
	GetWorktreePath(taskID string) (string, bool)
}

// PRLinker inspects and updates GitHub pull request metadata for the
// `ensure_pr_closes_issue` step. Implementations wrap `gh` CLI calls.
// Engine operates with a nil PRLinker — the step becomes a no-op when
// unset, so tests don't need to wire one.
type PRLinker interface {
	// GetClosingIssues returns issue numbers the PR's body is parsed by
	// GitHub as closing, scoped to the same repo as the PR. Also returns
	// the current PR body so callers can edit it without a second fetch.
	GetClosingIssues(repo string, prNumber int) (issues []int, body string, err error)
	// EditBody replaces the PR body.
	EditBody(repo string, prNumber int, body string) error
}

// CompletionInfo is passed to the OnComplete callback when a workflow finishes.
type CompletionInfo struct {
	TaskID     string
	WorkflowID string
	Variables  map[string]string
}

// Engine executes workflow definitions against tasks.
type Engine struct {
	store       *Store
	tasks       TaskProvider
	agents      AgentLauncher
	prLinker    PRLinker
	worktrees   WorktreeGetter
	onComplete  func(CompletionInfo)
	logger      *slog.Logger
	ctx         context.Context
	mu          sync.Mutex
	inflight    map[string]struct{} // taskID → step in flight (prevent double-advance)
	agentSteps  map[string]string   // agentID → stepID it was spawned for
	resumeError *logging.ErrorThrottle
}

// NewEngine creates a workflow engine.
func NewEngine(store *Store, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger) *Engine {
	return &Engine{
		store:       store,
		tasks:       tasks,
		agents:      agents,
		logger:      logger,
		ctx:         context.Background(),
		inflight:    make(map[string]struct{}),
		agentSteps:  make(map[string]string),
		resumeError: logging.NewErrorThrottle(),
	}
}

// SetContext binds a parent context to the engine. Shell steps use
// context.WithTimeout(parent, shellTimeout) so they are cancelled when
// the parent context is cancelled (e.g. on app shutdown).
func (e *Engine) SetContext(ctx context.Context) { e.ctx = ctx }

// Defs returns the workflow definition store.
func (e *Engine) Defs() *Store { return e.store }

// SetPRLinker wires an implementation of PRLinker used by the
// `ensure_pr_closes_issue` step. Leaving it unset makes the step a no-op.
func (e *Engine) SetPRLinker(l PRLinker) { e.prLinker = l }

// SetWorktreeGetter wires a WorktreeGetter used by the `verify_commits` step.
// Leaving it unset makes the step a no-op.
func (e *Engine) SetWorktreeGetter(g WorktreeGetter) { e.worktrees = g }

// SetOnComplete registers a callback fired when a workflow reaches the
// completed state. Used to clear external debounce trackers.
func (e *Engine) SetOnComplete(fn func(CompletionInfo)) { e.onComplete = fn }

// StartWorkflow assigns a workflow to a task and executes the first step.
func (e *Engine) StartWorkflow(taskID, workflowID string) error {
	return e.StartWorkflowWithVars(taskID, workflowID, nil)
}

// StartWorkflowWithVars assigns a workflow and seeds the execution with
// initial variables. Use the reserved WorkflowVarDir key to pass a
// pre-prepared working directory to run_agent steps.
func (e *Engine) StartWorkflowWithVars(taskID, workflowID string, vars map[string]string) error {
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

// ErrWorkflowAlreadyActive is returned by DispatchEvent when the target task
// already has a non-terminal workflow attached.
var ErrWorkflowAlreadyActive = fmt.Errorf("task already has an active workflow")

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

// AdvanceStep is called when an async step completes. It records the result,
// evaluates transitions, and executes the next step.
//
// No-ops (returns nil) when the workflow is already in a terminal state
// (completed/failed) or when the current step is empty. This prevents stale
// agent completions — e.g. agents spawned outside the workflow, or a
// double-delivered callback — from triggering "step not found" errors that
// would otherwise spam the log and re-persist the task file on every hit.
func (e *Engine) AdvanceStep(taskID string, output StepOutput) error {
	if !e.acquireInflight(taskID) {
		e.logger.Debug("workflow.advance.skip", "task_id", taskID, "reason", "already_advancing")
		return nil
	}
	// Released explicitly before executeSteps (below) so the dispatched
	// agent's completion callback isn't dropped as "already_advancing"
	// when it races with the outer call. Deferred release is idempotent
	// and covers every early-return path.
	defer e.releaseInflight(taskID)

	wfExec, def, currentStep, skip, err := e.loadAdvanceContext(taskID, output)
	if err != nil || skip {
		return err
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
			e.releaseInflight(taskID)
			return e.executeSteps(taskID, &def, currentStep, wfExec)
		}
		e.logger.Warn("workflow.retry.exhausted", "task_id", taskID, "step", output.StepID,
			"attempts", retries)
	}

	// Re-read task for latest state (agent may have changed tags/status).
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	t.Workflow = wfExec

	nextStep, err := e.resolveNext(taskID, &def, currentStep, wfExec, t)
	if err != nil {
		return err
	}
	if nextStep == nil {
		return nil // workflow completed
	}

	e.logger.Info("workflow.advance", "task_id", taskID, "from", output.StepID, "to", nextStep.ID)
	e.releaseInflight(taskID)
	return e.executeSteps(taskID, &def, nextStep, wfExec)
}

// acquireInflight attempts to mark a task as actively advancing. Returns
// false when another AdvanceStep call already owns the slot, in which case
// the caller must no-op rather than racing.
func (e *Engine) acquireInflight(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.inflight[taskID]; ok {
		return false
	}
	e.inflight[taskID] = struct{}{}
	return true
}

// releaseInflight clears the in-flight marker for a task.
func (e *Engine) releaseInflight(taskID string) {
	e.mu.Lock()
	delete(e.inflight, taskID)
	e.mu.Unlock()
}

// loadAdvanceContext validates and resolves the state needed by AdvanceStep.
// Returns skip=true (with nil error) for every legitimate no-op path: a
// terminal workflow, an empty step ID, a stale step (the ResumeStalled-race
// duplicate-agent guard), or an unexpected agent callback hitting a
// wait_human step without a human_action var set (defense-in-depth).
func (e *Engine) loadAdvanceContext(taskID string, output StepOutput) (*Execution, Definition, *Step, bool, error) {
	var emptyDef Definition
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return nil, emptyDef, nil, false, err
	}
	if t.Workflow == nil {
		return nil, emptyDef, nil, false, fmt.Errorf("task %s has no active workflow", taskID)
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		e.logger.Debug("workflow.advance.skip",
			"task_id", taskID, "reason", "workflow_terminal",
			"state", string(t.Workflow.State), "step_id", output.StepID)
		return nil, emptyDef, nil, true, nil
	}
	if output.StepID == "" {
		e.logger.Debug("workflow.advance.skip",
			"task_id", taskID, "reason", "empty_step_id",
			"state", string(t.Workflow.State))
		return nil, emptyDef, nil, true, nil
	}
	if output.StepID != t.Workflow.CurrentStep {
		e.logger.Debug("workflow.advance.skip",
			"task_id", taskID, "reason", "stale_step",
			"output_step", output.StepID, "current_step", t.Workflow.CurrentStep,
			"agent_id", output.AgentID)
		return nil, emptyDef, nil, true, nil
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return nil, emptyDef, nil, false, err
	}
	currentStep := def.StepByID(output.StepID)
	if currentStep == nil {
		return nil, emptyDef, nil, false, fmt.Errorf("step %s not found in workflow %s", output.StepID, def.ID)
	}

	if currentStep.Type == StepWaitHuman && output.AgentID != "" {
		if _, set := t.Workflow.Variables["human_action"]; !set {
			e.logger.Debug("workflow.advance.skip",
				"task_id", taskID, "reason", "wait_human_no_action",
				"step", output.StepID, "agent_id", output.AgentID)
			return nil, emptyDef, nil, true, nil
		}
	}

	return t.Workflow, def, currentStep, false, nil
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

// HandleStatusChange is called when a task's status transitions. If the
// current workflow step is a run_agent configured with a matching
// wait_for_status, the workflow advances past it. This is how interactive /
// conversational agents (which don't exit between turns) signal step
// completion: they update the task status via the CLI, the task manager
// fires the status-change hook, and the engine advances the workflow.
//
// Safe to call for any status change — no-ops when the current step does
// not declare wait_for_status or when the status does not match.
func (e *Engine) HandleStatusChange(taskID, newStatus string) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Debug("workflow.status-change.get", "task_id", taskID, "err", err)
		return
	}
	if t.Workflow == nil || t.Workflow.CurrentStep == "" {
		return
	}
	if t.Workflow.State != ExecWaiting && t.Workflow.State != ExecRunning {
		return
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil || step.Type != StepRunAgent {
		return
	}
	if step.Config.WaitForStatus == "" || step.Config.WaitForStatus != newStatus {
		return
	}

	e.logger.Info("workflow.status-advance",
		"task_id", taskID, "step", step.ID, "status", newStatus)

	if err := e.AdvanceStep(taskID, StepOutput{
		StepID: step.ID,
		Status: "completed",
		Output: "status:" + newStatus,
	}); err != nil {
		e.logger.Error("workflow.status-advance.err", "task_id", taskID, "err", err)
	}
}

// HandleAgentComplete is called when an agent finishes. It maps the agent
// back to the workflow step and advances.
//
// Silently skips (Debug log) when the task's workflow is already terminal or
// has no current step. Agents that were started outside the workflow engine
// (e.g. manual pr-fix retries, recovery spawns) land here on completion; the
// guard avoids the "step not found" error loop that followed workflow
// completion in older versions.
func (e *Engine) HandleAgentComplete(taskID string, c AgentCompletion) {
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		e.logger.Error("workflow.agent-complete.get", "task_id", taskID, "err", err)
		return
	}
	if t.Workflow == nil {
		e.logger.Debug("workflow.agent-complete.no-workflow", "task_id", taskID)
		return
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		e.logger.Debug("workflow.agent-complete.terminal",
			"task_id", taskID, "agent_id", c.AgentID, "state", string(t.Workflow.State))
		e.clearAgentStep(c.AgentID)
		return
	}
	if t.Workflow.CurrentStep == "" {
		e.logger.Debug("workflow.agent-complete.no-current-step",
			"task_id", taskID, "agent_id", c.AgentID, "state", string(t.Workflow.State))
		e.clearAgentStep(c.AgentID)
		return
	}

	// Resolve the step this agent was actually spawned for. Fallback to the
	// workflow's current step for agents that were never tracked (recovery
	// flows calling with synthetic IDs). The resolved ID is then checked
	// against the current step inside AdvanceStep to drop stale completions.
	spawnedStep, tracked := e.lookupAgentStep(c.AgentID)
	if !tracked {
		spawnedStep = t.Workflow.CurrentStep
	}

	status := "completed"
	if !c.Success {
		status = "failed"
	}

	if err := e.AdvanceStep(taskID, StepOutput{
		StepID:   spawnedStep,
		Status:   status,
		Output:   c.Result,
		AgentID:  c.AgentID,
		Provider: c.Provider,
	}); err != nil {
		e.logger.Error("workflow.agent-complete.advance", "task_id", taskID, "err", err)
	}
	e.clearAgentStep(c.AgentID)
}

// lookupAgentStep returns the stepID an agent was spawned for and whether it
// was tracked. Untracked agents fall back to the workflow's current step.
func (e *Engine) lookupAgentStep(agentID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	stepID, ok := e.agentSteps[agentID]
	return stepID, ok
}

// clearAgentStep removes the agent→step mapping. Safe to call for unknown IDs.
func (e *Engine) clearAgentStep(agentID string) {
	if agentID == "" {
		return
	}
	e.mu.Lock()
	delete(e.agentSteps, agentID)
	e.mu.Unlock()
}

// ResumeStalled finds tasks with running/waiting workflows where no agent
// is active, and attempts to re-execute the current step.
func (e *Engine) ResumeStalled() {
	tasks, err := e.tasks.ListTasks()
	if err != nil {
		e.logger.Error("workflow.resume-stalled.list", "err", err)
		return
	}

	for i := range tasks {
		t := &tasks[i]
		if t.Workflow == nil || t.Workflow.CurrentStep == "" {
			continue
		}
		switch t.Workflow.State {
		case ExecCompleted, ExecFailed:
			continue
		case ExecRunning, ExecWaiting:
			// fall through to resume logic
		}

		def, dErr := e.store.Get(t.Workflow.WorkflowID)
		if dErr != nil {
			continue
		}
		step := def.StepByID(t.Workflow.CurrentStep)
		if step == nil {
			continue
		}

		// Only resume run_agent steps where no agent is running.
		if step.Type != StepRunAgent {
			continue
		}
		if e.agents.HasRunningAgent(t.ID) {
			continue
		}
		// Skip tasks whose step is currently being dispatched. Interactive
		// spawns (worktree creation, rebase, agent process start) take
		// several seconds during which no agent is yet registered — without
		// this guard the ticker would spawn a duplicate and the second
		// agent's completion would corrupt the workflow at the wait_human
		// gate.
		e.mu.Lock()
		_, dispatching := e.inflight[t.ID]
		e.mu.Unlock()
		if dispatching {
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", "inflight", "step", step.ID)
			continue
		}

		e.logger.Info("workflow.resume-stalled", "task_id", t.ID, "step", step.ID)
		rErr := e.executeSteps(t.ID, &def, step, t.Workflow)
		e.resumeError.Log(e.logger, "workflow.resume-stalled.exec", t.ID, rErr, "task_id", t.ID)
	}
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
func (e *Engine) executeSteps(taskID string, def *Definition, step *Step, wfExec *Execution) error {
	visited := make(map[string]int) // stepID → first-seen iteration index
	for i := range maxSyncSteps {
		t, err := e.tasks.GetTask(taskID)
		if err != nil {
			return err
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
				return err
			}
		}

		// Async steps: execute and return. Callback (HandleAgentComplete/HandleHumanAction)
		// will call AdvanceStep later.
		switch step.Type {
		case StepRunAgent:
			return e.execRunAgent(taskID, step, wfExec, ctx)
		case StepWaitHuman:
			return e.execWaitHuman(taskID, step, wfExec)
		case StepSetStatus, StepCondition, StepShell, StepEnsurePRClosesIssue, StepVerifyCommits, StepLinkPRAndReview, StepEvaluate:
			// handled below as sync steps
		default:
			return fmt.Errorf("unknown step type %q", step.Type)
		}

		// Detect cycles: a sync step revisited without an async break means
		// the workflow loops forever. Return a CycleError instead of hitting
		// the generic maxSyncSteps limit.
		if firstAt, seen := visited[step.ID]; seen {
			return &CycleError{StepID: step.ID, At: i, FirstAt: firstAt}
		}
		visited[step.ID] = i

		// Sync steps: execute, record result, resolve next, loop.
		output, execErr := e.execSyncStep(taskID, step, wfExec, ctx, t)
		if execErr != nil {
			return execErr
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
			return err
		}
		t.Workflow = wfExec

		nextStep, nErr := e.resolveNext(taskID, def, step, wfExec, t)
		if nErr != nil {
			return nErr
		}
		if nextStep == nil {
			return nil // workflow completed
		}

		e.logger.Info("workflow.advance", "task_id", taskID, "from", step.ID, "to", nextStep.ID)
		step = nextStep
	}
	return fmt.Errorf("workflow exceeded max sync step depth (%d)", maxSyncSteps)
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
	case StepVerifyCommits:
		return e.execVerifyCommits(taskID, step, t)
	case StepLinkPRAndReview:
		return e.execLinkPRAndReview(taskID, step, wfExec, t)
	case StepEvaluate:
		return e.execEvaluate(taskID, step, wfExec)
	default:
		return StepOutput{}, fmt.Errorf("unknown step type %q", step.Type)
	}
}

// resolveNext evaluates transitions and returns the next step, or nil if workflow ends.
func (e *Engine) resolveNext(taskID string, def *Definition, current *Step, wfExec *Execution, t TaskInfo) (*Step, error) {
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
		return nil, tErr
	}

	if nextID == "" {
		now := time.Now().UTC()
		wfExec.State = ExecCompleted
		wfExec.CompletedAt = &now
		wfExec.CurrentStep = ""
		e.logger.Info("workflow.completed", "task_id", taskID, "workflow", def.ID)
		err := e.tasks.SetWorkflow(taskID, wfExec)
		if err == nil && e.onComplete != nil {
			e.onComplete(CompletionInfo{
				TaskID:     taskID,
				WorkflowID: def.ID,
				Variables:  wfExec.Variables,
			})
		}
		return nil, err
	}

	nextStep := def.StepByID(nextID)
	if nextStep == nil {
		return nil, fmt.Errorf("next step %s not found in workflow %s", nextID, def.ID)
	}

	wfExec.CurrentStep = nextStep.ID
	wfExec.State = ExecRunning
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return nil, err
	}
	return nextStep, nil
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

	// Track which step this agent was spawned for so HandleAgentComplete
	// can detect stale completions (e.g. duplicate agent from a ResumeStalled
	// race) rather than blindly crediting the current step.
	e.mu.Lock()
	e.agentSteps[agentID] = step.ID
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
		for i := len(wfExec.StepHistory) - 1; i >= 0; i-- {
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
func providerAvailable(provider string) bool {
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
		"SYNAPSE_TASK_ID="+ti.ID,
		"SYNAPSE_TASK_TITLE="+ti.Title,
		"SYNAPSE_TASK_STATUS="+ti.Status,
		"SYNAPSE_TASK_PROJECT="+ti.ProjectID,
		"SYNAPSE_TASK_BRANCH="+ti.Branch,
		fmt.Sprintf("SYNAPSE_TASK_PR=%d", ti.PRNumber),
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
//   - git command error (e.g. remote unreachable, detached HEAD)
//
// When the branch has no commits ahead of origin/main the task is flipped to
// human-required with reason "no commits pushed to branch" and the step
// returns "completed" so the workflow can route to end via a task.status
// transition condition rather than a failed step.
func (e *Engine) execVerifyCommits(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if e.worktrees == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree getter configured"}, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree for task"}, nil
	}

	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "log", "origin/main..HEAD", "--oneline")
	cmd.Dir = wtPath
	output, err := cmd.Output()
	if err != nil {
		e.logger.Warn("workflow.verify-commits.git-error", "task_id", taskID, "worktree", wtPath, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: git error: " + err.Error()}, nil
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

var prURLRe = regexp.MustCompile(`github\.com/[^/\s]+/[^/\s]+/pull/(\d+)`)

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

	// Path 2: Scan step history for a GitHub PR URL in agent output.
	for i := len(wfExec.StepHistory) - 1; i >= 0; i-- {
		rec := wfExec.StepHistory[i]
		if rec.Status != "completed" || rec.Output == "" {
			continue
		}
		if m := prURLRe.FindStringSubmatch(rec.Output); len(m) > 1 {
			n, err := strconv.Atoi(m[1])
			if err == nil && n > 0 {
				return setInReview(n, "agent result")
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
			if jsonErr := json.Unmarshal(out, &prs); jsonErr == nil && len(prs) == 1 {
				return setInReview(prs[0].Number, "gh pr list")
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
// This runs only when link_pr_and_review could not find a PR, so the task
// always ends in human-required here — there is no in-review path.
func (e *Engine) execEvaluate(taskID string, step *Step, wfExec *Execution) (StepOutput, error) {
	var last *StepRecord
	for i := len(wfExec.StepHistory) - 1; i >= 0; i-- {
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
