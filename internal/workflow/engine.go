package workflow

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/logging"
)

const (
	maxSyncSteps   = 100 // depth limit for synchronous step chains
	maxStepHistory = 50  // max step records kept per execution
	shellTimeout   = 30 * time.Second
)

// TaskInfo is the subset of task data the engine needs.
type TaskInfo struct {
	ID           string
	Title        string
	Status       string
	Tags         []string
	AgentMode    string
	ProjectID    string
	ProjectType  string
	PRNumber     int
	Branch       string
	Body         string
	Plan         string
	PlanCritique string
	CodeReview   string
	// PlanDrafts holds raw per-provider plans during dual-/N-provider planning.
	// Keys are parallel child step IDs (e.g. "plan_claude", "plan_codex").
	PlanDrafts map[string]string
	Issue      string
	Reviewed   bool
	Workflow   *Execution
}

// TaskProvider reads and updates tasks.
type TaskProvider interface {
	GetTask(id string) (TaskInfo, error)
	ListTasks() ([]TaskInfo, error)
	UpdateTaskStatus(id, status, reason string) error
	UpdateTaskPR(id string, prNumber int) error
	MarkTaskReviewed(id string) error
	SetWorkflow(id string, wf *Execution) error
	// WriteSidecar stores content as the named sidecar for the task. Used
	// by run_agent steps that declare import_sidecar so the engine can
	// ingest the agent's output file without depending on the agent's
	// sandbox being able to write to ~/.sybra/tasks/. Recognized kinds:
	//   "plan"          — final implementation plan
	//   "plan_critique" — plan-critic report
	//   "code_review"   — staff-code-review report
	//   "plan_draft.<name>" — raw per-provider plan during dual-/N-provider
	//       planning; <name> is typically the parallel child step ID. The
	//       engine derives <name> from the step ID when the YAML kind is
	//       a bare "plan_draft".
	WriteSidecar(id, kind, content string) error
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
	store           *Store
	tasks           TaskProvider
	agents          AgentLauncher
	prLinker        PRLinker
	worktrees       WorktreeGetter
	onComplete      func(CompletionInfo)
	logger          *slog.Logger
	ctx             context.Context
	mu              sync.Mutex
	inflightMutexes map[string]*sync.Mutex // taskID → advance serializer (parallel-aware)
	dispatching     map[string]struct{}    // taskID → dispatch in progress
	starting        map[string]struct{}    // taskID → StartWorkflowWithVars in progress
	humanAction     map[string]struct{}    // taskID → HandleHumanAction in progress
	agentSteps      map[string]string      // agentID → stepID it was spawned for
	resumeError     *logging.ErrorThrottle
}

// NewEngine creates a workflow engine.
func NewEngine(store *Store, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger) *Engine {
	return &Engine{
		store:           store,
		tasks:           tasks,
		agents:          agents,
		logger:          logger,
		ctx:             context.Background(),
		inflightMutexes: make(map[string]*sync.Mutex),
		dispatching:     make(map[string]struct{}),
		starting:        make(map[string]struct{}),
		humanAction:     make(map[string]struct{}),
		agentSteps:      make(map[string]string),
		resumeError:     logging.NewErrorThrottle(),
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
	defer e.releaseInflight(taskID)

	ctx, skip, err := e.loadAdvanceContext(taskID, output)
	if err != nil || skip {
		return err
	}
	wfExec, def, currentStep := ctx.WfExec, ctx.Def, ctx.Step

	// Parallel-child completion: route to the child-aware path. The parent
	// step's record + transitions are emitted only after every child has
	// terminated, so we never go through the single-step record path here.
	if ctx.ParallelParent != nil {
		return e.advanceParallelChild(taskID, &def, ctx.ParallelParent, currentStep, wfExec, output)
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
			return e.executeSteps(taskID, &def, currentStep, wfExec)
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

	nextStep, err := e.resolveNext(taskID, &def, currentStep, wfExec, t)
	if err != nil {
		return err
	}
	if nextStep == nil {
		return nil // workflow completed
	}

	e.logger.Info("workflow.advance", "task_id", taskID, "from", output.StepID, "to", nextStep.ID)
	return e.executeSteps(taskID, &def, nextStep, wfExec)
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

// HandleHumanAction processes approve/reject/input from the UI.
func (e *Engine) HandleHumanAction(taskID, action string, data map[string]string) error {
	// Serialize concurrent human actions per task so double-click races do not
	// both mutate workflow vars and attempt to advance the same wait_human step.
	e.mu.Lock()
	if _, busy := e.humanAction[taskID]; busy {
		e.mu.Unlock()
		return fmt.Errorf("task %s human action already in progress", taskID)
	}
	e.humanAction[taskID] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.humanAction, taskID)
		e.mu.Unlock()
	}()

	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if t.Workflow == nil || t.Workflow.State != ExecWaiting {
		return fmt.Errorf("task %s is not waiting for human action", taskID)
	}
	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return err
	}
	currentStep := def.StepByID(t.Workflow.CurrentStep)
	if currentStep == nil {
		return fmt.Errorf("step %s not found in workflow %s", t.Workflow.CurrentStep, def.ID)
	}
	if currentStep.Type != StepWaitHuman {
		return fmt.Errorf("task %s is not at a wait_human step", taskID)
	}
	if len(currentStep.Config.HumanActions) > 0 && !slices.Contains(currentStep.Config.HumanActions, action) {
		return fmt.Errorf("invalid human action %q for step %q", action, currentStep.ID)
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

	if c.Success {
		e.importSidecarIfConfigured(taskID, spawnedStep, t)
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
		// inflightMutexes is a non-blocking probe: TryLock distinguishes
		// "another goroutine currently holds the advance lock" from "free".
		// We only set dispatching when both the advance lock and prior
		// dispatching guard are free.
		mu := e.taskInflightMutex(t.ID)
		advancing := !mu.TryLock()
		if !advancing {
			mu.Unlock()
		}
		e.mu.Lock()
		_, dispatching := e.dispatching[t.ID]
		if !advancing && !dispatching {
			e.dispatching[t.ID] = struct{}{}
		}
		e.mu.Unlock()
		if advancing || dispatching {
			reason := "dispatching"
			if advancing {
				reason = "inflight"
			}
			e.logger.Debug("workflow.resume-stalled.skip",
				"task_id", t.ID, "reason", reason, "step", step.ID)
			continue
		}

		// Re-read to guard against stale snapshots from concurrent ResumeStalled
		// calls: by the time we acquire dispatching, a prior goroutine may have
		// already advanced the workflow past this step.
		fresh, fErr := e.tasks.GetTask(t.ID)
		if fErr != nil || fresh.Workflow == nil || fresh.Workflow.CurrentStep != t.Workflow.CurrentStep || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed {
			e.mu.Lock()
			delete(e.dispatching, t.ID)
			e.mu.Unlock()
			continue
		}

		e.logger.Info("workflow.resume-stalled", "task_id", t.ID, "step", step.ID)
		rErr := e.executeSteps(t.ID, &def, step, t.Workflow)
		e.mu.Lock()
		delete(e.dispatching, t.ID)
		e.mu.Unlock()
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
		case StepParallel:
			return e.execParallel(taskID, step, wfExec, ctx)
		case StepWaitHuman:
			return e.execWaitHuman(taskID, step, wfExec)
		case StepSetStatus, StepCondition, StepShell, StepEnsurePRClosesIssue, StepVerifyCommits, StepLinkPRAndReview, StepEvaluate, StepRequireSidecar:
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
		return e.execEvaluate(taskID, step, wfExec, t)
	case StepRequireSidecar:
		return e.execRequireSidecar(taskID, step, t)
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

func taskFields(t TaskInfo) map[string]string {
	fields := map[string]string{
		"task.id":         t.ID,
		"task.title":      t.Title,
		"task.status":     t.Status,
		"task.tags":       strings.Join(t.Tags, ","),
		"task.agent_mode": t.AgentMode,
		"task.project_id": t.ProjectID,
		"task.branch":     t.Branch,
		"task.reviewed":   strconv.FormatBool(t.Reviewed),
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
