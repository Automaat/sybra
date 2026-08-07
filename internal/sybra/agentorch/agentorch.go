// Package agentorch manages agent lifecycle: worktree setup, project
// assignment, and agent launching for a task.
package agentorch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/bgop"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/pressure"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/skillinvoke"
	"github.com/Automaat/sybra/internal/sybra/runenv"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

// ResolveExecution derives the effective mode, directory, permission mode, and
// whether project worktree setup should be skipped. Task mode is explicit; task
// type does not affect execution.
//
// "interactive" is coerced to "headless": it is no longer a dispatchable
// agent.RunConfig.Mode, but task.AgentModeInteractive is still a valid,
// load-only value on old task files (see task.validAgentModes), and
// simple-task-implement's `implement` step templates its mode straight off
// {{.Task.AgentMode}}. Without this, a legacy interactive task would dispatch
// RunConfig{Mode: "interactive"} and fail outright instead of running like
// every other task.
func ResolveExecution(t task.Task, hintMode, researchMachineDir string, cfg *config.Config) (mode, dir string, requirePerm, skipWorktree bool) {
	if hintMode == "interactive" {
		hintMode = "headless"
	}
	return hintMode, "", ResolvePermission(t, cfg), false
}

// ResolvePermission returns the effective require_permissions value for a task.
// Priority: task field > config default > true (safe default).
func ResolvePermission(t task.Task, cfg *config.Config) bool {
	if t.RequirePermissions != nil {
		return *t.RequirePermissions
	}
	return cfg.DefaultRequirePermissions()
}

// ResolveHeadlessPermissionMode returns the effective headless permission posture.
// Priority: task field > config default > "bypass".
// When the task carries an invalid value, an error is returned and the caller
// must abort the launch rather than silently falling back to bypass.
func ResolveHeadlessPermissionMode(t task.Task, cfg *config.Config) (string, error) {
	if t.HeadlessPermissionMode != "" {
		mode, err := config.NormalizeHeadlessPermissionMode(t.HeadlessPermissionMode)
		if err != nil {
			return "", fmt.Errorf("task %s: %w", t.ID, err)
		}
		return mode, nil
	}
	return cfg.DefaultHeadlessPermissionMode(), nil
}

// ResolveSandboxMode returns the effective OS-level process-sandbox posture
// for a task's agent processes ("off", "report", or "enforce").
// Priority: task.Sandbox escape hatch (false -> "off") > config default.
// A task can only opt OUT of the config default, never opt into a stricter
// posture than configured — Sandbox=true is a no-op, matching the intent of
// an escape hatch rather than a per-task posture override.
func ResolveSandboxMode(t task.Task, cfg *config.Config) string {
	if t.Sandbox != nil && !*t.Sandbox {
		return "off"
	}
	return cfg.DefaultSandboxMode()
}

// maxResumeZeroOutputStalls is the number of consecutive zero-output-stall
// runs a session may accumulate (see AgentRun.ResumeZeroOutputStall) before
// PickImplementationResumeSession treats it as poisoned and abandons it for
// an older session, or a fresh one.
const maxResumeZeroOutputStalls = 2

// PickImplementationResumeSession walks AgentRuns newest-first and returns
// the most recent session_id from a prior implementation run that belongs
// to the current workflow execution and provider — unless that session has
// been poisoned by repeated zero-output stalls (see below), in which case it
// is skipped in favor of an older, non-poisoned qualifying session.
//
// Three filters are applied:
//
//  1. Role must be implementation. Other roles (triage, plan, eval, …) own
//     their own session state, often run in a different cwd, and resuming
//     them from the implementation worktree makes claude bail with
//     error_during_execution before the prompt is ever sent. Empty Role is
//     allowed only for legacy runs predating the orchestrator role-recording
//     fix; new runs always carry Role explicitly.
//  2. StartedAt must be at or after workflowStart. A previous workflow
//     execution may have left an aborted implementation run with a
//     session_id that no longer exists in claude's session store. Resuming
//     it would make claude exit with "No conversation found", cost $0,
//     and verify_commits flip the task to human-required without ever
//     running the implementation prompt. workflowStart=zero disables the
//     time filter (useful for callers that have no execution context).
//  3. Provider must match the provider about to dispatch. A session id is
//     only valid within the CLI session store of the provider that created
//     it — a codex session_id means nothing to claude and vice versa. On a
//     mid-workflow provider failover (e.g. codex → claude retry), a run on
//     the new provider must never adopt the old provider's session_id, or
//     the retry fails instantly with "No conversation found" before the
//     prompt is ever sent. Empty run.Provider is allowed only for legacy
//     runs predating provider recording; provider="" disables the filter
//     (useful for callers that have no provider context).
//
// Circuit breaker: contiguity for "N consecutive stalls" is defined over the
// qualifying subsequence (i.e. runs that already passed the three filters
// above), not the raw run list — a run belonging to a different role/provider
// or predating workflowStart never breaks a streak, it is simply invisible to
// this logic. Walking that subsequence newest-first, runs are grouped by
// SessionID: the first (newest) group is the resume candidate. If it
// accumulates maxResumeZeroOutputStalls consecutive ResumeZeroOutputStall
// runs with no intervening non-stall run, it is poisoned and skipped — the
// walk continues into the next distinct SessionID group as the new candidate.
// A qualifying run of the current (not-yet-poisoned) candidate session that
// is NOT a zero-output stall resolves the candidate immediately: a session
// that has ever produced output is never treated as poisoned, no matter what
// stalls happened earlier in its history.
func PickImplementationResumeSession(runs []task.AgentRun, workflowStart time.Time, dispatchProvider string) string {
	candidate := ""
	stallStreak := 0
	poisoned := false

	for i := range slices.Backward(runs) {
		run := &runs[i]
		if run.SessionID == "" {
			continue
		}
		if run.Role != "" && run.Role != string(agent.RoleImplementation) {
			continue
		}
		if !workflowStart.IsZero() && run.StartedAt.Before(workflowStart) {
			continue
		}
		if dispatchProvider != "" && run.Provider != "" && run.Provider != dispatchProvider {
			continue
		}
		if run.SessionID != candidate {
			// About to cross into an older, distinct session group. If the
			// current candidate accumulated stalls but never reached the
			// poison threshold, it is still valid and newer — resolve it now
			// rather than discarding it for a staler session. Mirrors the
			// post-loop fallback so it applies at every group boundary too.
			if candidate != "" && !poisoned {
				return candidate
			}
			candidate = run.SessionID
			stallStreak = 0
			poisoned = false
		}
		if poisoned {
			continue
		}
		if !run.ResumeZeroOutputStall {
			return candidate
		}
		stallStreak++
		if stallStreak >= maxResumeZeroOutputStalls {
			poisoned = true
		}
	}
	if candidate != "" && !poisoned {
		return candidate
	}
	return ""
}

// Orchestrator manages agent lifecycle: worktree setup, project
// assignment, and agent launching for a task.
type Orchestrator struct {
	tasks     *task.Manager
	projects  *project.Store
	agents    *agent.Manager
	worktrees *worktree.Manager
	cfg       *config.Config
	sandboxes *sandbox.Manager
	bgops     *bgop.Tracker
	logger    *slog.Logger
	audit     *audit.Logger
	// pressureGate defers new work when the local host is short on disk,
	// memory, or CPU headroom. Late-bound via SetPressureGate so startup can
	// construct it from config after New returns. Nil disables gating.
	pressureGate *pressure.Gate
	// queue is the admission queue a workflow implementation dispatch falls
	// back to when the agent pool is saturated (see StartAgentWithAssignment's
	// admissionGate). Late-bound via SetQueue once agentqueue.New succeeds at
	// startup; nil disables admission gating entirely (dispatch behaves
	// exactly as it did before this feature landed) — every read site
	// nil-guards for this reason.
	queue *agentqueue.Queue
	// runenv certifies the concrete worktree, Git admin area, provider, and
	// host posture immediately before Run. Nil is retained for construction
	// tests and degraded startup; App always late-binds it before dispatch.
	runenv *runenv.Service
	// conflictRecovery turns a worktree-prep rebase conflict into an autonomous
	// conflict pr-fix instead of a human escalation. Wired via SetConflictRecovery
	// in wireServices after the review.Handler exists; nil keeps the
	// escalate-to-human fallback. sandboxes/bgops/conflictRecovery are all set
	// after New() returns, once the file watcher (initFileWatcher) may already
	// be running — every read site nil-guards for this reason. Do not remove
	// the nil guards even if app.go's init order changes to close the window;
	// the ordering is easy to regress silently.
	conflictRecovery func(taskID string) bool
	ctx              context.Context
	// sloReport is the latest autonomy SLO compliance verdict, late-bound via
	// SetSLOReport (wired from evaluation.Service.Deps.OnSLOReport). nil
	// until the first evaluation tick — handleSaturatedDispatch's throttle
	// treats nil the same as a non-exhausted budget, so a config with
	// SLO.ThrottleOnBudgetExhausted enabled but no evaluation service running
	// never throttles.
	sloReport atomic.Pointer[evaluation.SLOReport]
}

// SetSLOReport late-binds the most recent SLO compliance verdict. Called
// from evaluation.Service's per-tick callback; safe for concurrent use.
func (o *Orchestrator) SetSLOReport(r evaluation.SLOReport) {
	o.sloReport.Store(&r)
}

// New constructs an Orchestrator. Sandboxes and Bgops are late-bound fields,
// wired in after construction once those subsystems exist, via SetSandboxes
// and SetBgops.
func New(
	tasks *task.Manager,
	projects *project.Store,
	agents *agent.Manager,
	al *audit.Logger,
	logger *slog.Logger,
	worktrees *worktree.Manager,
	cfg *config.Config,
) *Orchestrator {
	return &Orchestrator{
		audit:     al,
		logger:    logger,
		tasks:     tasks,
		projects:  projects,
		agents:    agents,
		worktrees: worktrees,
		cfg:       cfg,
	}
}

// SetSandboxes late-binds the sandbox manager once it exists.
func (o *Orchestrator) SetSandboxes(sandboxes *sandbox.Manager) {
	o.sandboxes = sandboxes
}

// SetBgops late-binds the background-operation tracker once it exists.
func (o *Orchestrator) SetBgops(bgops *bgop.Tracker) {
	o.bgops = bgops
}

// SetPressureGate late-binds the local resource-pressure admission gate.
func (o *Orchestrator) SetPressureGate(gate *pressure.Gate) {
	o.pressureGate = gate
}

// SetConflictRecovery late-binds the autonomous conflict-recovery callback
// once the review.Handler that implements it exists.
func (o *Orchestrator) SetConflictRecovery(fn func(taskID string) bool) {
	o.conflictRecovery = fn
}

// SetRunEnvironment late-binds pre-dispatch certification.
func (o *Orchestrator) SetRunEnvironment(service *runenv.Service) {
	o.runenv = service
}

// SetQueue late-binds the admission queue once agentqueue.New succeeds at
// startup (see App.initWorkflowEngine). Leaving it unset (nil) — e.g. because
// construction failed — disables admission gating: workflow implementation
// dispatch behaves exactly as it did before this feature landed.
func (o *Orchestrator) SetQueue(q *agentqueue.Queue) {
	o.queue = q
}

// SetContext late-binds the app root context so dispatch-path worktree/sandbox
// prep is cancelled on shutdown instead of running detached to its own timeout.
func (o *Orchestrator) SetContext(ctx context.Context) { o.ctx = ctx }

func (o *Orchestrator) baseCtx() context.Context {
	if o.ctx != nil {
		return o.ctx
	}
	return context.Background()
}

// Sandboxes returns the late-bound sandbox manager, for callers (e.g.
// startup-wiring assertions) that need to verify it was wired correctly.
func (o *Orchestrator) Sandboxes() *sandbox.Manager {
	return o.sandboxes
}

// Bgops returns the late-bound background-operation tracker, for callers
// (e.g. startup-wiring assertions) that need to verify it was wired
// correctly.
func (o *Orchestrator) Bgops() *bgop.Tracker {
	return o.bgops
}

// HasConflictRecovery reports whether the autonomous conflict-recovery
// callback has been wired, without exposing the callback itself.
func (o *Orchestrator) HasConflictRecovery() bool {
	return o.conflictRecovery != nil
}

// Cfg returns the shared config, for callers (e.g. app_workflow.go's
// agentAdapter) that need to resolve task/config-derived settings the
// orchestrator itself doesn't expose a verb for.
func (o *Orchestrator) Cfg() *config.Config {
	return o.cfg
}

// Worktrees returns the shared worktree manager, for callers that need
// worktree operations the orchestrator itself doesn't expose a verb for.
func (o *Orchestrator) Worktrees() *worktree.Manager {
	return o.worktrees
}

// Projects returns the shared project store, for callers that need project
// lookups the orchestrator itself doesn't expose a verb for.
func (o *Orchestrator) Projects() *project.Store {
	return o.projects
}

// Logger returns the shared logger, for callers that need to log in the
// same stream as the orchestrator's own logging.
func (o *Orchestrator) Logger() *slog.Logger {
	return o.logger
}

// LogAudit records a structured audit event; a nil audit logger silently no-ops.
func (o *Orchestrator) LogAudit(eventType, taskID, agentID string, data map[string]any) {
	audit.LogEvent(o.audit, o.logger, eventType, taskID, agentID, data)
}

// resolveDispatchProvider predicts the provider a Run call for this
// assignment will actually dispatch to, so resume-session selection can be
// scoped to it. assignment.Provider carries the provider explicitly picked
// for this dispatch (e.g. a cross-provider retry); an empty assignment
// defers to the manager's configured default. Resolving through
// agent.Manager.ResolveProvider — the same gating logic Run itself uses —
// reflects health-gate/limit-gate failover before a resumable session is
// picked; otherwise a session from the requested provider could be handed
// to a run that actually dispatches to a different one.
func (o *Orchestrator) resolveDispatchProvider(taskID string, assignment workflow.AgentAssignment) string {
	resolved, err := o.agents.ResolveProvider(agent.RunConfig{
		TaskID:                  taskID,
		Provider:                assignment.Provider,
		DisableProviderFailover: assignment.ExperimentID != "",
	})
	if err == nil {
		return resolved
	}
	if assignment.Provider != "" {
		return assignment.Provider
	}
	return o.agents.DefaultProvider()
}

func (o *Orchestrator) assignmentModel(assigned string) string {
	if o.cfg == nil {
		return FirstNonEmpty(assigned, "sonnet")
	}
	return FirstNonEmpty(assigned, o.cfg.Agent.Model, "sonnet")
}

// SandboxEnvIfRunning returns the sandbox env vars only when a sandbox is
// already running for the task; it never starts one. Sandboxes are started
// lazily by the testing phase (test-runner role), so implementation/review
// agents inherit one only if testing left it up — they never spin a cluster.
func (o *Orchestrator) SandboxEnvIfRunning(taskID string) []string {
	if o.sandboxes == nil {
		return nil
	}
	if inst := o.sandboxes.Get(taskID); inst != nil {
		return inst.EnvVars()
	}
	return nil
}

// SandboxEnv resolves the extra environment variables a task's configured
// sandbox injects into its agent subprocess, starting the sandbox on demand.
// Returns nil when no sandbox applies or startup fails (a failed start is
// logged, not fatal — the agent runs without the sandbox env). Called from the
// testing phase so the per-task sandbox spins up only when tests actually run.
func (o *Orchestrator) SandboxEnv(taskID, dir string, t task.Task) []string {
	if o.sandboxes == nil || t.ProjectID == "" {
		return nil
	}
	proj, pErr := o.projects.Get(t.ProjectID)
	if pErr != nil || proj.Sandbox == nil {
		return nil
	}
	inst := o.sandboxes.Get(taskID)
	if inst == nil {
		// context.Background(): SandboxEnv is called from agentAdapter.StartAgent,
		// which implements workflow.AgentDispatcher — a fixed interface signature
		// with no ctx parameter (see the comment on the PrepareForTask call in
		// app_workflow.go).
		newInst, startErr := o.sandboxes.Start(context.Background(), taskID, dir, proj.Sandbox)
		if startErr != nil {
			o.logger.Warn("sandbox.start.failed", "task_id", taskID, "err", startErr)
			return nil
		}
		inst = newInst
	}
	return inst.EnvVars()
}

// PrependSupervisorSteer consumes a pending watchdog headless-nudge steer for
// taskID: it clears the one-shot SupervisorSteer field and returns prompt with
// the correction prepended. Returns prompt unchanged when none is pending.
// Called at the head of the headless re-dispatch entry points (this orchestrator,
// the workflow run_agent path, and the pr-fix agent) so the first agent resumed
// after a nudge carries the correction and later ones do not. The orchestrator
// resume loops (ResumeStalled then RestartStaleInProgress) run sequentially on a
// single goroutine and each gates on no-running-agent before dispatching, so the
// read-then-clear is not raced by a concurrent dispatcher for the same task.
//
// The prompt is steered ONLY after the clear succeeds, so a failed clear leaves
// the steer pending (the error is returned, the prompt is unchanged) rather than
// applying it twice — preserving the one-shot contract. A start that then fails
// loses the nudge, which is recoverable: the watchdog re-nudges if the resumed
// agent loops again.
func PrependSupervisorSteer(tasks *task.Manager, taskID, prompt string) (string, error) {
	t, err := tasks.Get(taskID)
	if err != nil {
		// Cannot read the task → cannot consume a steer; dispatch unsteered and
		// let the caller log. Any pending steer stays for a later dispatch.
		return prompt, err
	}
	steer := strings.TrimSpace(t.SupervisorSteer)
	if steer == "" {
		return prompt, nil
	}
	if _, uErr := tasks.Update(taskID, task.Update{SupervisorSteer: task.Ptr("")}); uErr != nil {
		return prompt, fmt.Errorf("clear supervisor steer: %w", uErr)
	}
	return "Supervisor course-correction: " + steer + "\n\n" + prompt, nil
}

type startOptions struct {
	admissionGate bool
	manualDrain   bool
	// outputSchema is the workflow step's OutputSchema, threaded onto the
	// implementation RunConfig so a run_agent step with output_schema reaches
	// the provider with --json-schema / --output-schema. Empty for the
	// manual/drain entry points, which carry no schema.
	outputSchema string
}

// StartAgent is the manual/direct dispatch entry point (App.StartAgent,
// recovery.RestartStaleInProgress). Saturated non-interactive starts are
// persisted to the manual queue and return a synthetic queued agent instead of
// surfacing a pool-busy error to the caller.
func (o *Orchestrator) StartAgent(taskID, mode, prompt string, includeTaskDescription, oneShot bool) (*agent.Agent, error) {
	ag, _, err := o.startAgent(o.baseCtx(), taskID, mode, prompt, includeTaskDescription, oneShot, "", workflow.AgentAssignment{}, startOptions{})
	return ag, err
}

// StartQueuedManualItem replays a previously queued manual start once the app
// drain has observed available capacity. Unlike StartAgent, it never silently
// re-queues on a transient pool-busy race; the caller re-offers the original
// item with its original Enqueued timestamp so durable intent is not dropped.
func (o *Orchestrator) StartQueuedManualItem(ctx context.Context, it agentqueue.Item) (*agent.Agent, error) {
	if !it.Manual {
		return nil, fmt.Errorf("task %s: queued manual replay requires manual queue item", it.TaskID)
	}
	mode := FirstNonEmpty(it.Mode, "headless")
	ag, _, err := o.startAgent(ctx, it.TaskID, mode, it.Prompt, it.IncludeTaskDescription, false, "", workflow.AgentAssignment{}, startOptions{manualDrain: true})
	return ag, err
}

// logSandboxEscapeHatch records an operator-visible signal (audit log +
// structured log) when a task's Sandbox escape hatch overrides the config
// default to "off", so unrestricted-write dispatches are discoverable
// without waiting for an incident. No-op when the escape hatch is unused.
func (o *Orchestrator) logSandboxEscapeHatch(taskID string, t task.Task) {
	if t.Sandbox == nil || *t.Sandbox {
		return
	}
	// An empty reason is itself the signal: `"reason":""` in the audit line
	// marks an unexplained bypass as plainly as a separate flag would.
	reason := strings.TrimSpace(t.SandboxOffReason)
	o.logger.Warn("agent.sandbox.escape_hatch", "task_id", taskID, "reason", reason)
	o.LogAudit(audit.EventAgentSandboxDisabled, taskID, "", map[string]any{
		"configured_default": o.cfg.DefaultSandboxMode(),
		"reason":             reason,
	})
}

// resolveDispatchDir prepares (or reuses) the working directory for a
// project-backed task dispatch. When skipWT is true, dir is returned as given.
// Otherwise it auto-assigns a project if needed, optionally resets the
// worktree for a clean retry, and prepares the task's worktree, returning
// the (possibly project-assigned) task and its worktree directory.
func (o *Orchestrator) resolveDispatchDir(ctx context.Context, t task.Task, taskID, cleanRetryRef string, skipWT bool, dir string, claim *agent.DispatchClaim, reservation *agent.CapacityReservation) (task.Task, string, error) {
	if skipWT {
		return t, dir, nil
	}
	if o.worktrees == nil {
		return t, fallbackDispatchDir(dir), nil
	}
	var assignErr error
	t, assignErr = o.AutoAssignProject(t)
	if assignErr != nil {
		return t, "", assignErr
	}
	if t.ProjectID == "" {
		return t, "", fmt.Errorf("task %s has no project_id: refusing to start agent without isolated worktree: %w", taskID, workflow.ErrNoProjectAssigned)
	}
	if cleanRetryRef != "" {
		if resetErr := o.resetWorktreeForCleanRetry(ctx, t, cleanRetryRef); resetErr != nil {
			return t, "", resetErr
		}
	}
	opID, onPhase := o.startWorktreeOp("Preparing worktree: "+t.Title, t.ProjectID, taskID)
	d, wtErr := o.worktrees.PrepareForTask(ctx, t, onPhase)
	if wtErr != nil {
		o.failWorktreeOp(opID, wtErr)
		// A tracked agent is still live in this worktree (see
		// worktree.PrepareForTask's hasAgent guard) — this is a benign
		// timing collision with a stale "no agent running" read
		// upstream, not a real worktree conflict. Wait rather than
		// escalate; the agent's own completion (or a later ResumeStalled
		// tick, once it is genuinely idle) drives the workflow forward.
		if errors.Is(wtErr, worktreeerr.ErrAgentRunning) {
			return t, "", workflow.ErrDispatchInFlight
		}
		// o.conflictRecovery (wired to review.Handler.RecoverStaleBranchConflict)
		// synchronously starts the branch-conflict-fix workflow, whose "fix"
		// step dispatches a new agent for this SAME taskID through this same
		// StartAgentWithAssignment choke point. Release the outer claim and the
		// held pool slot first — we are bailing out of this dispatch regardless
		// of the recovery result — or that nested dispatch can observe either
		// the claim as still held (ErrDispatchInFlight) or the pool as still
		// saturated (ErrAgentPoolBusy) without ever starting the fix agent.
		claim.Release()
		reservation.Release()
		if _, recovered := MarkRebaseBlockedWithRecoveryResult(o.tasks, taskID, wtErr, o.logger, o.conflictRecovery); recovered {
			return t, "", workflow.ErrDispatchInFlight
		}
		return t, "", fmt.Errorf("worktree required for project task: %w", wtErr)
	}
	o.completeWorktreeOp(opID)
	return t, d, nil
}

// translatePoolBusy maps the agent-package concurrency sentinel onto
// workflow.ErrAgentPoolBusy so every caller of this package's dispatch
// methods — the workflow engine's agentAdapter, the recovery loop's
// RestartStaleInProgress, or a future caller — sees the same benign,
// self-healing signal regardless of entry point. Without this translation at
// the source, a caller that reaches agent.Manager.Run through this package
// without its own wrapping (e.g. recovery.Recovery, which invokes
// StartAgent/StartPRFixAgent directly) lets the raw agent.ErrMaxConcurrentReached
// through to workflow.ClassifyAgentStartError, which then falls to the
// generic "agent start failed" branch and writes a scary, non-suppressed
// status_reason for a condition that resolves itself once a pool slot frees.
func translatePoolBusy(err error) error {
	if errors.Is(err, agent.ErrMaxConcurrentReached) {
		return fmt.Errorf("%w: %w", workflow.ErrAgentPoolBusy, err)
	}
	return err
}

// StartAgentWithAssignment is the workflow implementation dispatch path
// (agentAdapter.StartAgent for the implementation role). Gated by admission:
// when the agent pool is saturated it offers the task to the queue and
// returns workflow.ErrAgentPoolBusy instead of dispatching, so run_agent
// parks ExecWaiting and ResumeStalled retries once a slot frees.
func (o *Orchestrator) StartAgentWithAssignment(taskID, mode, prompt string, includeTaskDescription, oneShot bool, cleanRetryRef, outputSchema string, assignment workflow.AgentAssignment) (*agent.Agent, string, error) {
	return o.startAgent(o.baseCtx(), taskID, mode, prompt, includeTaskDescription, oneShot, cleanRetryRef, assignment, startOptions{admissionGate: true, outputSchema: outputSchema})
}

func (o *Orchestrator) startAgent(ctx context.Context, taskID, mode, prompt string, includeTaskDescription, oneShot bool, cleanRetryRef string, assignment workflow.AgentAssignment, opts startOptions) (*agent.Agent, string, error) {
	// Serialize dispatch per task. Held across the whole start — including the
	// multi-second worktree prep below, during which the agent is not yet
	// registered — so a concurrent dispatcher (recovery loop, ResumeStalled,
	// a manual start) cannot observe "no running agent" and launch a duplicate
	// on the same worktree. workflow.ErrDispatchInFlight is benign: the holder
	// will produce the task's agent.
	claim, ok := o.agents.TryClaimDispatch(taskID)
	if !ok {
		return nil, "", workflow.ErrDispatchInFlight
	}
	// claim.Release is idempotent so resolveDispatchDir can release it early
	// (before triggering a nested same-task dispatch, e.g. branch-conflict
	// recovery) without this defer double-releasing on return.
	defer claim.Release()

	// Consume a pending watchdog headless-nudge steer (no-op when none). Held
	// within the dispatch claim so the read-then-clear is serialized per task.
	// On a clear failure the steer stays pending and we dispatch unsteered.
	if steered, sErr := PrependSupervisorSteer(o.tasks, taskID, prompt); sErr != nil {
		o.logger.Warn("supervisor-steer.consume", "task_id", taskID, "err", sErr)
	} else {
		prompt = steered
	}

	t, err := o.tasks.Get(taskID)
	if err != nil {
		return nil, "", err
	}
	// An umbrella tracker task runs no agent — it only rolls up its children.
	// Refuse here, the single dispatch choke point, so no path (workflow,
	// recovery, manual start) can launch an agent against it.
	if t.TaskType == task.TaskTypeUmbrella {
		return nil, "", fmt.Errorf("task %s is an umbrella tracker; it runs no agent", taskID)
	}
	if err := o.enforceTaskCostBudget(t); err != nil {
		return nil, "", err
	}
	researchDir := ""
	if o.cfg != nil {
		researchDir = o.cfg.Agent.ResearchMachineDir
	}
	effMode, dir, requirePerm, skipWT := ResolveExecution(t, mode, researchDir, o.cfg)
	// effMode is always headless (interactive is coerced away in
	// ResolveExecution), so implementation dispatches never bypass the
	// concurrency/pressure gate — legacy interactive tasks are treated as
	// ordinary headless runs.
	if o.pressureGate != nil {
		if admit, reason := o.pressureGate.Admit(); !admit {
			o.LogAudit(audit.EventAgentDeferredPressure, taskID, "", map[string]any{"reason": reason})
			return nil, "", fmt.Errorf("%w: %s", workflow.ErrResourcePressure, reason)
		}
	}

	reservation, ag, baselineRef, err, handled := o.handleSaturatedDispatch(t, taskID, effMode, prompt, includeTaskDescription, skipWT, opts)
	if handled {
		if ag != nil {
			return ag, baselineRef, nil
		}
		if err != nil {
			return nil, baselineRef, err
		}
		return nil, "", fmt.Errorf("task %s: saturated dispatch returned neither agent nor error", taskID)
	}
	if reservation != nil {
		defer reservation.Release()
	}

	t, dir, dirErr := o.resolveDispatchDir(ctx, t, taskID, cleanRetryRef, skipWT, dir, claim, reservation)
	if dirErr != nil {
		return nil, "", dirErr
	}
	if dir == "" {
		return nil, "", fmt.Errorf("task %s: no working dir resolved (skipWorktree=%v) — refusing to run agent in Sybra cwd", taskID, skipWT)
	}

	baselineRef = CurrentWorktreeHead(ctx, dir)

	var workflowStart time.Time
	if t.Workflow != nil {
		workflowStart = t.Workflow.StartedAt
	}
	dispatchProvider := o.resolveDispatchProvider(taskID, assignment)
	resumeSessionID := PickImplementationResumeSession(t.AgentRuns, workflowStart, dispatchProvider)
	model := o.assignmentModel(assignment.Model)

	posture, postureErr := ResolveHeadlessPermissionMode(t, o.cfg)
	if postureErr != nil {
		return nil, "", postureErr
	}
	return o.runImplementationAgent(ctx, taskID, prompt, includeTaskDescription, oneShot, skipWT, dir, effMode, requirePerm, posture, baselineRef, resumeSessionID, model, t, assignment, opts, reservation)
}

// runImplementationAgent builds the RunConfig for an implementation dispatch,
// launches it, and translates a launch failure through the same
// capacity-race / provider-gate handling startAgent used inline before this
// was split out to satisfy funlen.
func (o *Orchestrator) runImplementationAgent(ctx context.Context, taskID, prompt string, includeTaskDescription, oneShot, skipWT bool, dir, effMode string, requirePerm bool, posture, baselineRef, resumeSessionID, model string, t task.Task, assignment workflow.AgentAssignment, opts startOptions, reservation *agent.CapacityReservation) (*agent.Agent, string, error) {
	extraEnv := o.SandboxEnvIfRunning(taskID)
	fullPrompt := BuildTaskStartPrompt(t, prompt, includeTaskDescription)
	o.logSandboxEscapeHatch(taskID, t)
	runCfg := o.implementationRunConfig(implementationRunParams{
		taskID: taskID, t: t, effMode: effMode, prompt: prompt, fullPrompt: fullPrompt, dir: dir,
		assignment: assignment, model: model, requirePerm: requirePerm, posture: posture,
		oneShot:         oneShot,
		resumeSessionID: resumeSessionID, extraEnv: extraEnv, opts: opts,
	})
	if err := o.certifyRunEnvironment(ctx, taskID, t, agent.RoleImplementation, &runCfg); err != nil {
		return nil, "", err
	}
	var (
		ag  *agent.Agent
		err error
	)
	if reservation != nil {
		ag, err = o.agents.RunWithCapacityReservation(runCfg, reservation)
	} else {
		ag, err = o.agents.Run(runCfg)
	}
	if err != nil {
		if o.runenv != nil && runenv.IsEnvironmentFailure(err) {
			o.runenv.InvalidateTask(taskID)
		}
		o.handleProviderGateStartError(taskID, err)
		if ag, baselineRef, capErr, handled := o.handleCapacityRace(err, t, taskID, effMode, prompt, includeTaskDescription, skipWT, opts); handled {
			if ag != nil {
				return ag, baselineRef, nil
			}
			if capErr != nil {
				return nil, baselineRef, capErr
			}
			return nil, "", fmt.Errorf("task %s: capacity race handler returned neither agent nor error", taskID)
		}
		return nil, "", translatePoolBusy(err)
	}
	o.recordImplAgentStart(ag, t, taskID, effMode, posture, requirePerm, oneShot, fullPrompt)
	return ag, baselineRef, nil
}

func (o *Orchestrator) certifyRunEnvironment(ctx context.Context, taskID string, t task.Task, role agent.Role, cfg *agent.RunConfig) error {
	if o.runenv == nil {
		return nil
	}
	resolvedProvider, err := o.agents.ResolveProvider(*cfg)
	if err != nil {
		return err
	}
	cloneDir := ""
	cloneGeneration := ""
	if t.ProjectID != "" && o.projects != nil {
		p, projectErr := o.projects.Get(t.ProjectID)
		if projectErr != nil {
			return projectErr
		}
		cloneDir = p.ClonePath
		cloneGeneration = p.CloneGeneration
	}
	scratchRoots, err := o.agents.CertificationScratchRoots(taskID)
	if err != nil {
		return err
	}
	action := string(role) + ".dispatch"
	if role == "" {
		action = "implementation.dispatch"
	}
	_, err = o.runenv.Certify(ctx, runenv.Request{
		TaskID: taskID, ProjectID: t.ProjectID, Action: action,
		WorkDir: cfg.Dir, ScratchRoots: scratchRoots, CloneDir: cloneDir, CloneGeneration: cloneGeneration, TaskBranch: t.Branch,
		Provider: resolvedProvider, SandboxMode: cfg.SandboxMode,
		SigningPolicy: project.NormalizeSigningPolicy(o.cfg.CommitSigning()),
		Requirements:  role.CapabilityRequirements(action),
		ConfigVersion: fmt.Sprintf("%s|%s", cfg.SandboxMode, o.cfg.CommitSigning()),
	})
	return err
}

// implementationRunParams collects startAgent's locals for
// implementationRunConfig, which builds the agent.RunConfig for the
// implementation-role dispatch. Kept as one struct rather than a long
// positional parameter list — mirrors the shape of the RunConfig it feeds.
type implementationRunParams struct {
	taskID          string
	t               task.Task
	effMode         string
	prompt          string
	fullPrompt      string
	dir             string
	assignment      workflow.AgentAssignment
	model           string
	requirePerm     bool
	posture         string
	oneShot         bool
	resumeSessionID string
	extraEnv        []string
	opts            startOptions
}

func (o *Orchestrator) implementationRunConfig(p implementationRunParams) agent.RunConfig {
	return agent.RunConfig{
		TaskID:                  p.taskID,
		Name:                    p.t.Title,
		Role:                    agent.RoleImplementation,
		Mode:                    p.effMode,
		Prompt:                  p.fullPrompt,
		AllowedTools:            p.t.AllowedTools,
		Dir:                     p.dir,
		Provider:                p.assignment.Provider,
		Model:                   p.model,
		ExperimentID:            p.assignment.ExperimentID,
		VariantID:               p.assignment.VariantID,
		RoutingReason:           p.assignment.RoutingReason,
		AssignmentUnit:          p.assignment.AssignmentUnit,
		AssignmentKey:           p.assignment.AssignmentKey,
		DecisionVersion:         p.assignment.DecisionVersion,
		DisableProviderFailover: p.assignment.ExperimentID != "",
		RequestedSkill:          requestedWorkflowSkill(p.prompt),
		RequirePermissions:      p.requirePerm,
		HeadlessPermissionMode:  p.posture,
		OneShot:                 p.oneShot,
		ResumeSessionID:         p.resumeSessionID,
		ExtraEnv:                p.extraEnv,
		MaxTurns:                p.t.MaxTurns,
		// Always an implementation run (a code-author role, Role.AuthorsCode),
		// so the task-level opt-in applies unconditionally here — see
		// agentAdapter.StartAgent for the role-gated equivalent used by
		// every other role (verifier roles must never fork).
		ForkSubagent:    p.t.ForkSubagent,
		SandboxMode:     ResolveSandboxMode(p.t, o.cfg),
		ReasoningEffort: FirstNonEmpty(p.assignment.ReasoningEffort, p.t.ReasoningEffort, ResolveRoleEffort(agent.RoleImplementation, o.cfg)),
		// Always an implementation run — prime it with the NOTES.md scratchpad.
		SeedWorkingMemory: true,
		OutputSchema:      p.opts.outputSchema,
	}
}

func requestedWorkflowSkill(prompt string) string {
	names := skillinvoke.InvokedNames(prompt)
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

func (o *Orchestrator) handleSaturatedDispatch(t task.Task, taskID, mode, prompt string, includeTaskDescription, skipWT bool, opts startOptions) (*agent.CapacityReservation, *agent.Agent, string, error, bool) {
	// Admission gate: workflow implementation dispatches park their token in
	// the workflow-owned queue, while saturated manual headless starts persist
	// their replay intent in the manual queue and return a synthetic queued
	// agent.
	// SLO throttle: only ever narrows opts.admissionGate dispatch (workflow
	// implementation, StartAgentWithAssignment) — the one path that expands
	// autonomous concurrency. StartAgent's manual/recovery entry point
	// (App.StartAgent, recovery.RestartStaleInProgress) never sets
	// admissionGate, so recovery and manual operator dispatch are exempt by
	// construction, not by a special case here.
	// Reserve capacity against the SLO-throttled ceiling (0 = no extra
	// ceiling) so the reservation is reservation-aware and atomic: a burst of
	// concurrent admission-gated dispatches can no longer each observe a stale
	// live-only count and collectively overshoot the halved ceiling.
	sloLimit := 0
	if opts.admissionGate {
		sloLimit = o.sloConcurrencyLimit()
	}
	if reservation, ok := o.agents.TryHoldCapacityWithLimit(sloLimit); ok {
		return reservation, nil, "", nil, false
	}
	if o.queue == nil {
		// The SLO throttle actively narrowed admission (sloLimit > 0) and the
		// hold failed against the halved ceiling, but the raw pool may still
		// have room. Without a queue to park in, falling through to unthrottled
		// dispatch would let admission-gated work overshoot the halved ceiling
		// straight up to the raw cap — defeating the throttle in exactly the
		// degraded-startup mode (nil queue) where the system is already
		// unhealthy. Reject instead of bypassing.
		if opts.admissionGate && sloLimit > 0 {
			return nil, nil, "", workflow.ErrAgentPoolBusy, true
		}
		return nil, nil, "", nil, false
	}
	switch {
	case opts.admissionGate:
		return nil, nil, "", o.admitQueueFullOrEnqueue(t, taskID), true
	case opts.manualDrain:
		return nil, nil, "", workflow.ErrAgentPoolBusy, true
	default:
		ag, baselineRef, err := o.enqueueManualStart(t, taskID, mode, prompt, includeTaskDescription, skipWT)
		return nil, ag, baselineRef, err, true
	}
}

// sloConcurrencyLimit returns the concurrency ceiling workflow-driven
// implementation dispatch must be held to right now, or 0 when the SLO
// throttle imposes no extra ceiling. The throttle engages only when the
// operator has opted into agent.evaluation.slo.throttle_on_budget_exhausted
// AND the autonomy SLO error budget is exhausted. Default-off: a nil
// sloReport (SLO service disabled, or no tick has run yet) or the flag unset
// returns 0. A caller passes the result to TryHoldCapacityWithLimit so the
// halved ceiling is enforced reservation-aware and atomically, closing the
// TOCTOU gap between a live-only precheck and the reservation-counting hold.
func (o *Orchestrator) sloConcurrencyLimit() int {
	if o.cfg == nil || !o.cfg.Evaluation.SLO.ThrottleOnBudgetExhausted {
		return 0
	}
	report := o.sloReport.Load()
	if report == nil || report.ErrorBudgetRemaining > 0 {
		return 0
	}
	ceiling := effectiveMaxConcurrent(o.cfg.Agent.MaxConcurrent, true, true)
	if ceiling <= 0 {
		// Unlimited configured pool: nothing to narrow against.
		return 0
	}
	return ceiling
}

// effectiveMaxConcurrent returns the concurrency ceiling workflow-driven
// implementation dispatch may claim right now. configured is
// agent.max_concurrent (<=0 means unlimited, returned unchanged — the
// throttle only ever narrows a real cap, never invents one). When throttle is
// enabled and the SLO error budget is exhausted, the ceiling is halved
// (floor 1) so autonomous concurrency contracts instead of continuing to
// expand into a regression. A pure function so the halving policy is
// unit-testable without constructing an Orchestrator.
func effectiveMaxConcurrent(configured int, throttle, budgetExhausted bool) int {
	if configured <= 0 || !throttle || !budgetExhausted {
		return configured
	}
	return max(configured/2, 1)
}

func (o *Orchestrator) handleCapacityRace(runErr error, t task.Task, taskID, mode, prompt string, includeTaskDescription, skipWT bool, opts startOptions) (*agent.Agent, string, error, bool) {
	// Queue-nil degraded mode still relies on registerRunningAgent's final cap
	// check, so a concurrent dispatch can steal the last slot after startAgent's
	// earlier preflight. Re-enqueue the same way as the normal saturation path
	// when the queue exists.
	if o.queue == nil || !errors.Is(runErr, agent.ErrMaxConcurrentReached) {
		return nil, "", nil, false
	}
	_, ag, baselineRef, err, handled := o.handleSaturatedDispatch(t, taskID, mode, prompt, includeTaskDescription, skipWT, opts)
	return ag, baselineRef, err, handled
}

// admitQueueFullOrEnqueue offers taskID's implementation dispatch to the
// admission queue and reports the outcome as an agent-start error: present in
// the queue afterward (freshly queued or refreshed in place) maps to
// workflow.ErrAgentPoolBusy, the benign/transient sentinel that parks the
// run_agent step in ExecWaiting for ResumeStalled to retry. Rejected (max
// depth reached, or an unsafe TaskID) maps to a normal, non-transient error
// instead — the task must not be parked as if it were queued when it isn't.
func (o *Orchestrator) admitQueueFullOrEnqueue(t task.Task, taskID string) error {
	if o.enqueueImplementation(t, taskID) {
		return workflow.ErrAgentPoolBusy
	}
	return fmt.Errorf("task %s: agent pool full and admission queue rejected the task (max depth reached or unsafe task id)", taskID)
}

func (o *Orchestrator) enqueueManualStart(t task.Task, taskID, mode, prompt string, includeTaskDescription, skipWT bool) (*agent.Agent, string, error) {
	var err error
	t, err = o.ensureQueueableManualTask(t, taskID, skipWT)
	if err != nil {
		return nil, "", err
	}
	if o.queue == nil {
		return nil, "", workflow.ErrAgentPoolBusy
	}
	o.queue.Offer(agentqueue.Item{
		TaskID:                 taskID,
		Role:                   string(agent.RoleImplementation),
		Class:                  string(agent.RoleImplementation.WorkloadClass()),
		Priority:               t.Priority,
		Status:                 t.Status,
		Manual:                 true,
		Mode:                   mode,
		Prompt:                 prompt,
		IncludeTaskDescription: includeTaskDescription,
	})
	snap := o.queue.Snapshot()
	for i := range snap {
		if snap[i].TaskID != taskID || !snap[i].Manual {
			continue
		}
		return syntheticQueuedAgent(t, mode), "", nil
	}
	return nil, "", fmt.Errorf("task %s: agent pool full and manual queue rejected the task (max depth reached or unsafe task id)", taskID)
}

func (o *Orchestrator) ensureQueueableManualTask(t task.Task, taskID string, skipWT bool) (task.Task, error) {
	if skipWT {
		return t, nil
	}
	if o.worktrees == nil {
		return t, nil
	}
	assigned, err := o.AutoAssignProject(t)
	if err != nil {
		return t, err
	}
	if assigned.ProjectID == "" {
		return assigned, fmt.Errorf("task %s has no project_id: refusing to queue agent without isolated worktree: %w", taskID, workflow.ErrNoProjectAssigned)
	}
	return assigned, nil
}

// enqueueImplementation offers an implementation-dispatch item to the
// admission queue and confirms it landed. Offer's bool return conflates
// "freshly queued" and "already queued, refreshed in place" (both false) with
// "rejected" (max depth or unsafe TaskID) — Snapshot disambiguates so callers
// only treat the first two as a successful admission.
func (o *Orchestrator) enqueueImplementation(t task.Task, taskID string) bool {
	o.queue.Offer(agentqueue.Item{
		TaskID:   taskID,
		Role:     string(agent.RoleImplementation),
		Class:    string(agent.RoleImplementation.WorkloadClass()),
		Priority: t.Priority,
		Status:   t.Status,
	})
	snap := o.queue.Snapshot()
	for i := range snap {
		if snap[i].TaskID == taskID {
			return true
		}
	}
	return false
}

func syntheticQueuedAgent(t task.Task, mode string) *agent.Agent {
	now := time.Now()
	return &agent.Agent{
		ID:          "queued-" + t.ID,
		TaskID:      t.ID,
		Mode:        mode,
		State:       agent.StateQueued,
		Name:        t.Title,
		Project:     t.ProjectID,
		StartedAt:   now,
		LastEventAt: now,
	}
}

// taskCumulativeCostUSD sums CostUSD across every AgentRun a task has ever
// had, regardless of provider or outcome. Used to enforce
// agent.max_task_cost_usd, which — unlike the per-run MaxCostUSD guardrail —
// must not reset on retry.
func taskCumulativeCostUSD(runs []task.AgentRun) float64 {
	var total float64
	for i := range runs {
		total += runs[i].CostUSD
	}
	return total
}

// CheckTaskCostBudget re-exports the cumulative task cost-budget check
// (agent.max_task_cost_usd) for dispatch paths that bypass
// StartAgentWithAssignment — e.g. workflow.execBestOfN, whose attempts and
// judge step dispatch through the direct-dispatch StartAgent branch, which
// does not itself enforce the budget. Returns workflow.ErrTaskCostExceeded
// (wrapped) when the task has already spent its budget.
func (o *Orchestrator) CheckTaskCostBudget(taskID string) error {
	t, err := o.tasks.Get(taskID)
	if err != nil {
		return err
	}
	return o.enforceTaskCostBudget(t)
}

func (o *Orchestrator) enforceTaskCostBudget(t task.Task) error {
	if o.cfg == nil || o.cfg.Agent.MaxTaskCostUSD <= 0 {
		return nil
	}
	spent := taskCumulativeCostUSD(t.AgentRuns)
	if spent < o.cfg.Agent.MaxTaskCostUSD {
		return nil
	}
	return fmt.Errorf("%w: $%.2f spent across %d run(s), limit $%.2f",
		workflow.ErrTaskCostExceeded, spent, len(t.AgentRuns), o.cfg.Agent.MaxTaskCostUSD)
}
func (o *Orchestrator) handleProviderGateStartError(taskID string, err error) {
	switch {
	case errors.Is(err, provider.ErrProviderUnhealthy):
		// Gate block leaves no running agent. Flip the task back to todo so
		// watchdog / restart-stale loops don't chase a ghost in-progress row.
		o.revertToTodoAfterGateBlock(taskID, "task.revert-on-gate")
		o.LogAudit(audit.EventProviderGateBlocked, taskID, "", map[string]any{"err": err.Error()})
		o.logger.Info("agent.start.gated", "task_id", taskID, "err", err)
	case errors.Is(err, agent.ErrProviderModelIncompatible):
		o.revertToTodoAfterGateBlock(taskID, "task.revert-on-model")
		o.LogAudit(audit.EventProviderModelIncompatible, taskID, "", map[string]any{"err": err.Error()})
		o.logger.Warn("agent.start.model_incompatible", "task_id", taskID, "err", err)
	default:
		return
	}
}

// revertToTodoAfterGateBlock flips taskID back to todo after a start attempt
// was blocked before any agent process ran, so watchdog / restart-stale
// loops don't chase a ghost in-progress row. It submits the revert as a
// transition intent CAS'd on the task still being in-progress: if a
// concurrent writer (retry, human action, another gate) already moved the
// task somewhere else, that decision wins and this revert is a no-op rather
// than clobbering it. logSuffix distinguishes the two call sites in error logs.
func (o *Orchestrator) revertToTodoAfterGateBlock(taskID, logSuffix string) {
	cur, err := o.tasks.Get(taskID)
	if err != nil {
		o.logger.Error(logSuffix, "task_id", taskID, "err", err)
		return
	}
	if cur.Status != task.StatusInProgress {
		return
	}
	expected := cur.Status
	if _, err := o.tasks.Apply(task.TransitionIntent{
		TaskID:         taskID,
		ToStatus:       task.StatusTodo,
		Actor:          "agentorch.provider_gate",
		ExpectedStatus: &expected,
	}); err != nil {
		var conflict *task.ConflictError
		if errors.As(err, &conflict) {
			// Someone else already moved the task off in-progress between
			// our read and this write — their decision wins.
			return
		}
		o.logger.Error(logSuffix, "task_id", taskID, "err", err)
	}
}

func (o *Orchestrator) resetWorktreeForCleanRetry(ctx context.Context, t task.Task, ref string) error {
	target, reset, err := o.worktrees.ResetForRetry(ctx, t, "", ref)
	if err != nil {
		o.logger.Warn("worktree.clean-retry.reset", "task_id", t.ID, "path", target, "ref", ref, "err", err)
		return err
	}
	if reset {
		o.logger.Info("worktree.clean-retry.reset", "task_id", t.ID, "path", target, "ref", ref)
	}
	return nil
}

// MarkRebaseBlocked handles a worktree-prep rebase failure. A rebase abort means
// the task branch conflicts with base — exactly the case the conflict pr-fix
// agent resolves (it checks out the PR head without rebasing and resolves
// conflicts in-agent). So when recoverConflict re-dispatches that fix, the task
// is NOT stranded on a human; only when there is no linked PR to fix (or its
// retry budget is spent) do we fall back to human-required. recoverConflict may
// be nil (callers without a PR-monitor handle), which preserves the old
// escalate-to-human behaviour.
//
// Before parking, re-probes the task's linked PR (if any): a rebase failure
// can mean the local branch merely diverged from a remote an external bot
// already force-pushed a green fix onto, in which case there is genuinely
// nothing left to fix and human-required would be a false park. Only reached
// when recoverConflict declined/is absent — the common case re-dispatches a
// conflict pr-fix, whose own agent run flows through execRoutePRFixResult's
// equivalent re-probe.
func MarkRebaseBlocked(tasks *task.Manager, taskID string, err error, logger *slog.Logger, recoverConflict func(string) bool) bool {
	if !errors.Is(err, worktree.ErrRebaseFailed) {
		return false
	}
	// A disk-space-caused rebase failure is not a content conflict — skip
	// conflict recovery and the PR re-probe (both assume a genuine merge
	// conflict) and park directly with the real root cause. Routing this
	// through recovery would waste an agent run before falling back to the
	// same human-required park anyway. See #1856.
	if worktreeerr.IsDiskSpaceError(err) {
		logger.Warn("worktree.rebase-block.disk-space", "task_id", taskID)
		if _, uerr := tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: task.StatusBlocked,
			Actor:    "agentorch.mark_rebase_blocked.disk_space",
			Extra: task.Update{
				StatusReason:    task.Ptr(worktreeerr.DiskSpaceExhaustedReason),
				Escalation:      task.MachineFailure("runenv.disk_space_exhausted", worktreeerr.DiskSpaceExhaustedReason),
				AutonomyOutcome: task.QuarantinedOutcome(),
			},
		}); uerr != nil {
			logger.Error("worktree.rebase-block.status", "task_id", taskID, "err", uerr)
		}
		return true
	}
	if recoverConflict != nil {
		if recoverConflict(taskID) {
			logger.Info("worktree.rebase-block.handled", "task_id", taskID)
			return true
		}
	}
	if reason, resolved := rebaseBlockedPRAlreadyResolved(tasks, taskID); resolved {
		if _, uerr := tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: task.StatusInReview,
			Actor:    "agentorch.mark_rebase_blocked.pr_resolved",
			Extra: task.Update{
				StatusReason: task.Ptr(reason),
			},
		}); uerr != nil {
			logger.Error("worktree.rebase-block.status", "task_id", taskID, "err", uerr)
		}
		logger.Info("worktree.rebase-block.already-resolved", "task_id", taskID)
		return true
	}
	if recoverConflict != nil {
		// recoverConflict may have already parked the task human-required with a
		// specific reason (e.g. an exhausted retry-attempt count) before
		// declining — see review.Handler.markConflictRecoveryExhausted. Respect
		// that instead of overwriting it with the generic reason below, so an
		// operator (or the automated human-review agent) can tell an exhausted
		// recovery loop apart from a fresh, first-time conflict. This must run
		// after the remote PR re-probe above, because an externally resolved PR
		// should still flip back to in-review instead of staying parked.
		if t, err := tasks.Get(taskID); err == nil &&
			(t.Status == task.StatusHumanRequired || t.Status == task.StatusBlocked) && t.StatusReason != "" {
			return true
		}
	}
	if _, uerr := tasks.Apply(task.TransitionIntent{
		TaskID:   taskID,
		ToStatus: task.StatusBlocked,
		Actor:    "agentorch.mark_rebase_blocked.parked",
		Extra: task.Update{
			StatusReason:    task.Ptr(worktreeerr.RebaseBlockedReason),
			Escalation:      task.MachineFailure("git.rebase_repair_exhausted", worktreeerr.RebaseBlockedReason),
			AutonomyOutcome: task.QuarantinedOutcome(),
		},
	}); uerr != nil {
		logger.Error("worktree.rebase-block.status", "task_id", taskID, "err", uerr)
	}
	return true
}

// fetchPRStateForRebaseBlock is overridable in tests to avoid shelling out to `gh`.
var fetchPRStateForRebaseBlock = github.FetchPRState

// rebaseBlockedPRAlreadyResolved re-probes the task's linked PR when a rebase
// failure has no autonomous recovery path. A fetch error or an unlinked PR
// falls through to the normal human-required park.
func rebaseBlockedPRAlreadyResolved(tasks *task.Manager, taskID string) (reason string, resolved bool) {
	t, err := tasks.Get(taskID)
	if err != nil || t.ProjectID == "" || t.PRNumber == 0 {
		return "", false
	}
	state, err := fetchPRStateForRebaseBlock(t.ProjectID, t.PRNumber)
	if err != nil || !state.Resolved() {
		return "", false
	}
	return fmt.Sprintf("worktree rebase-blocked: PR #%d already resolved on remote", t.PRNumber), true
}

// MarkRebaseBlockedWithRecoveryResult behaves like MarkRebaseBlocked but also
// reports whether recoverConflict actually recovered the task, so callers can
// distinguish "handled by escalating to human" from "handled by an autonomous
// conflict recovery" (the latter should not also surface an error to the caller).
func MarkRebaseBlockedWithRecoveryResult(tasks *task.Manager, taskID string, err error, logger *slog.Logger, recoverConflict func(string) bool) (handled, recovered bool) {
	if recoverConflict == nil {
		return MarkRebaseBlocked(tasks, taskID, err, logger, nil), false
	}
	wrappedRecover := func(id string) bool {
		recovered = recoverConflict(id)
		return recovered
	}
	return MarkRebaseBlocked(tasks, taskID, err, logger, wrappedRecover), recovered
}

// RecoverFromWorktreePrepFailure marks taskID rebase-blocked using this
// Orchestrator's own logger and late-bound conflict-recovery callback,
// wrapping MarkRebaseBlockedWithRecoveryResult for callers outside this
// package that don't have direct access to either.
func (o *Orchestrator) RecoverFromWorktreePrepFailure(tasks *task.Manager, taskID string, err error) (handled, recovered bool) {
	return MarkRebaseBlockedWithRecoveryResult(tasks, taskID, err, o.logger, o.conflictRecovery)
}

// recordImplAgentStart emits the agent.started audit event and persists the
// initial AgentRun record for an implementation agent.
func (o *Orchestrator) recordImplAgentStart(ag *agent.Agent, t task.Task, taskID, effMode, posture string, requirePerm, oneShot bool, fullPrompt string) {
	skipPerm := !requirePerm && len(t.AllowedTools) == 0
	// Hash the prompt the agent was actually dispatched with (ag.Prompt, set by
	// Run after prepareRunConfig appended NOTES.md, the background-task
	// guardrail, and any injected/fallback skill instructions), NOT the
	// pre-preparation fullPrompt. This is the same canonical prompt the provider
	// render summary (agent.prompt_rendered) is computed against, so the shared
	// prompt_hash uniquely identifies the dispatched prompt variant: re-running
	// the task after editing NOTES.md or changing the resolved skill source
	// yields a different hash, as it must. Run/newRunningAgent already stamps
	// this same value centrally for every run; the re-stamp here is idempotent
	// and keeps the agent.started payload correct for callers (and tests) that
	// invoke recordImplAgentStart on an agent built outside that path.
	ag.SetPromptHash(skillattr.HashSourceID(ag.Prompt))
	o.LogAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{
		"mode": effMode, "title": t.Title, "role": string(agent.RoleImplementation), "task_type": string(t.TaskType), "provider": ag.Provider,
		"model": ag.Model, "experiment_id": ag.ExperimentID, "variant_id": ag.VariantID,
		"allowed_tools": t.AllowedTools, "require_permissions": requirePerm, "skip_permissions": skipPerm,
		"permission_posture": posture, "prompt_hash": ag.GetPromptHash(),
	})
	var nextStatus *task.Status
	if t.Status != task.StatusInProgress {
		nextStatus = task.Ptr(task.StatusInProgress)
	}
	if err := o.tasks.AddRunWithStatus(taskID, task.AgentRun{
		AgentID:                 ag.ID,
		Role:                    string(agent.RoleImplementation),
		Mode:                    effMode,
		Provider:                ag.Provider,
		Model:                   ag.Model,
		ExperimentID:            ag.ExperimentID,
		VariantID:               ag.VariantID,
		RoutingReason:           ag.RoutingReason,
		AssignmentUnit:          ag.AssignmentUnit,
		AssignmentKey:           ag.AssignmentKey,
		DecisionVersion:         ag.DecisionVersion,
		ReasoningEffort:         ag.ReasoningEffort,
		RequestedSkill:          ag.RequestedSkill,
		SkillExecutionMode:      ag.SkillExecutionMode,
		ResolvedSkillSourceHash: ag.ResolvedSkillSourceHash,
		SkillConformance:        ag.SkillConformance,
		OneShot:                 oneShot,
		State:                   string(agent.StateRunning),
		StartedAt:               ag.StartedAt,
		Prompt:                  fullPrompt,
	}, nextStatus); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
}

func BuildTaskStartPrompt(t task.Task, prompt string, includeTaskDescription bool) string {
	prompt = strings.TrimSpace(prompt)
	if !includeTaskDescription {
		return prompt
	}
	base := fmt.Sprintf("# Task: %s\n\n%s", t.Title, t.Body)
	if len(t.Attachments) > 0 {
		lines := make([]string, 0, len(t.Attachments)+2)
		lines = append(lines, "## Attachments")
		for _, att := range t.Attachments {
			lines = append(lines, fmt.Sprintf("- %s | %d bytes | %s | %s", att.FileName, att.SizeBytes, att.ContentType, att.Path))
		}
		base += "\n\n" + strings.Join(lines, "\n")
	}
	if prompt == "" {
		return base
	}
	return base + "\n\n---\n\n" + prompt
}

// AutoAssignProject assigns a project to a project-less task that needs one
// to dispatch. It prefers the operator-configured agent.default_project_id
// (checked against the registered set, so a stale/typo'd ID is a no-op
// rather than a bogus assignment); absent that, it falls back to the sole
// registered project when exactly one is registered. No-op otherwise —
// notably when the task already has a project, or when default_project_id
// is unset and more than one project is registered (ambiguous, needs either
// config or a human to assign one).
func (o *Orchestrator) AutoAssignProject(t task.Task) (task.Task, error) {
	if t.ProjectID != "" || o.projects == nil {
		return t, nil
	}
	projects, err := o.projects.List()
	if err != nil {
		o.logger.Warn("auto-assign-project.list-projects", "task_id", t.ID, "err", err)
		return t, fmt.Errorf("list registered projects for auto-assignment: %w", err)
	}
	projectID := ""
	if def := o.defaultProjectID(); def != "" {
		for i := range projects {
			if projects[i].ID == def {
				projectID = def
				break
			}
		}
	}
	if projectID == "" && len(projects) == 1 {
		projectID = projects[0].ID
	}
	if projectID == "" {
		return t, nil
	}
	assigned := t
	assigned.ProjectID = projectID
	if _, err := o.tasks.Update(t.ID, task.Update{ProjectID: task.Ptr(assigned.ProjectID)}); err != nil {
		o.logger.Error("auto-assign-project", "task_id", t.ID, "err", err)
		return t, fmt.Errorf("persist auto-assigned project %q for task %s: %w", projectID, t.ID, err)
	} else {
		o.logger.Info("auto-assign-project", "task_id", t.ID, "project", assigned.ProjectID)
	}
	return assigned, nil
}

// defaultProjectID returns the configured agent.default_project_id, or ""
// when unset or config is unavailable (e.g. in tests that build an
// Orchestrator without a config).
func (o *Orchestrator) defaultProjectID() string {
	if o.cfg == nil {
		return ""
	}
	return strings.TrimSpace(o.cfg.Agent.DefaultProjectID)
}

// StartPRFixAgent starts a headless agent to address review comments on
// the task's PR. Named "pr-fix:" so handleAgentComplete routes it correctly.
func (o *Orchestrator) StartPRFixAgent(taskID string) error {
	// Same per-task dispatch serialization as StartAgent — a pr-fix dispatch
	// must not race a concurrent implementation/recovery dispatch.
	claim, ok := o.agents.TryClaimDispatch(taskID)
	if !ok {
		return workflow.ErrDispatchInFlight
	}
	defer claim.Release()

	t, err := o.tasks.Get(taskID)
	if err != nil {
		return err
	}
	if err := o.enforceTaskCostBudget(t); err != nil {
		return err
	}

	researchDir := ""
	if o.cfg != nil {
		researchDir = o.cfg.Agent.ResearchMachineDir
	}
	effMode, dir, requirePerm, skipWT := ResolveExecution(t, t.AgentMode, researchDir, o.cfg)
	if !skipWT {
		if o.worktrees == nil {
			return fmt.Errorf("task %s: refusing to start pr-fix agent without isolated worktree manager: %w", taskID, workflow.ErrNoProjectAssigned)
		}
		t, err = o.AutoAssignProject(t)
		if err != nil {
			return err
		}
		if t.ProjectID == "" {
			return fmt.Errorf("task %s has no project_id: refusing to start pr-fix agent without isolated worktree: %w", taskID, workflow.ErrNoProjectAssigned)
		}
		opID, onPhase := o.startWorktreeOp("Preparing worktree: "+t.Title, t.ProjectID, taskID)
		d, wtErr := o.worktrees.PrepareForTask(o.baseCtx(), t, onPhase)
		if wtErr != nil {
			o.failWorktreeOp(opID, wtErr)
			return fmt.Errorf("worktree required: %w", wtErr)
		}
		o.completeWorktreeOp(opID)
		dir = d
	}
	if dir == "" {
		return fmt.Errorf("task %s: no working dir resolved (skipWorktree=%v) — refusing to run agent in Sybra cwd", taskID, skipWT)
	}

	posture, postureErr := ResolveHeadlessPermissionMode(t, o.cfg)
	if postureErr != nil {
		return postureErr
	}

	prompt := BuildPRFixPrompt(t, o.logger)
	if steered, sErr := PrependSupervisorSteer(o.tasks, taskID, prompt); sErr != nil {
		o.logger.Warn("supervisor-steer.consume", "task_id", taskID, "err", sErr)
	} else {
		prompt = steered
	}
	o.logSandboxEscapeHatch(taskID, t)
	runCfg := agent.RunConfig{
		TaskID:                 taskID,
		Name:                   agent.RolePRFix.AgentName(t.Title),
		Role:                   agent.RolePRFix,
		Mode:                   effMode,
		Prompt:                 prompt,
		AllowedTools:           t.AllowedTools,
		Dir:                    dir,
		Model:                  "sonnet",
		RequirePermissions:     requirePerm,
		HeadlessPermissionMode: posture,
		SandboxMode:            ResolveSandboxMode(t, o.cfg),
		// pr-fix is a code-author role — keep the NOTES.md contract airtight so
		// an adopted (handoff) worktree's scratchpad carries through.
		SeedWorkingMemory: agent.RolePRFix.AuthorsCode(),
	}
	if err := o.certifyRunEnvironment(o.baseCtx(), taskID, t, agent.RolePRFix, &runCfg); err != nil {
		return err
	}
	ag, err := o.agents.Run(runCfg)
	if err != nil {
		return translatePoolBusy(err)
	}

	skipPerm := !requirePerm && len(t.AllowedTools) == 0
	o.LogAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{
		"mode": effMode, "title": t.Title, "role": "pr-fix", "task_type": string(t.TaskType), "provider": ag.Provider,
		"allowed_tools": t.AllowedTools, "require_permissions": requirePerm, "skip_permissions": skipPerm,
		"permission_posture": posture, "prompt_hash": ag.GetPromptHash(),
	})
	if err := o.tasks.AddRun(taskID, task.AgentRun{
		AgentID: ag.ID, Role: string(agent.RolePRFix), Mode: effMode,
		Provider: ag.Provider, Model: ag.Model, RoutingReason: ag.RoutingReason,
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
		Prompt: prompt,
	}); err != nil {
		o.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return nil
}

func fallbackDispatchDir(dir string) string {
	if dir != "" {
		return dir
	}
	return os.TempDir()
}

// BuildPRFixPrompt constructs the prompt for a PR fix agent.
// If the task has an associated PR, it fetches review context (URL, branch,
// review comments) and includes it so the agent amends the existing PR rather
// than starting from scratch.
func BuildPRFixPrompt(t task.Task, logger *slog.Logger) string {
	base := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\nFix the issues raised in the PR review. Push the changes when done.\n\nNever weaken, skip, delete, comment out, or hardcode tests, snapshots, or fixtures to make checks pass, and never edit CI config to neuter a gate. Fix the underlying code; tampering is detected and blocks the task.", t.Title, t.Body)
	if t.PRNumber == 0 || t.ProjectID == "" {
		return base
	}

	prCtx, err := github.FetchPRContext(t.ProjectID, t.PRNumber)
	if err != nil {
		logger.Warn("pr-fix.fetch-context", "pr", t.PRNumber, "err", err)
		// Fall back to minimal context from task fields.
		branch := t.Branch
		if branch == "" {
			branch = "unknown"
		}
		return fmt.Sprintf("%s\n\n## PR Context\n- PR: #%d (https://github.com/%s/pull/%d)\n- Branch: `%s`\n\nCheck out the branch and push amended commits to the same branch.", base, t.PRNumber, t.ProjectID, t.PRNumber, branch)
	}

	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\n## PR Context\n")
	fmt.Fprintf(&sb, "- PR: #%d (%s)\n", t.PRNumber, prCtx.URL)
	fmt.Fprintf(&sb, "- Branch: `%s`\n", prCtx.Branch)
	sb.WriteString("\nDo NOT open a new PR. Push commits to the existing branch `")
	sb.WriteString(prCtx.Branch)
	sb.WriteString("`.\n")

	if len(prCtx.Comments) > 0 {
		sb.WriteString("\n## Review Comments to Address\n")
		for i, c := range prCtx.Comments {
			fmt.Fprintf(&sb, "\n### Comment %d", i+1)
			if c.Author != "" {
				fmt.Fprintf(&sb, " (by @%s)", c.Author)
			}
			if c.Path != "" {
				fmt.Fprintf(&sb, " on `%s`", c.Path)
			}
			sb.WriteString("\n")
			sb.WriteString(c.Body)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// startWorktreeOp starts a bgop for worktree preparation and returns the op ID
// and a phase-update callback. Returns empty string and nil when Bgops is nil.
func (o *Orchestrator) startWorktreeOp(label, projectID, taskID string) (opID string, onPhase func(string)) {
	if o.bgops == nil {
		return "", nil
	}
	opID = o.bgops.Start(bgop.TypeWorktreePrep, label, projectID, taskID)
	onPhase = func(phase string) { o.bgops.UpdatePhase(opID, phase) }
	return opID, onPhase
}

func (o *Orchestrator) completeWorktreeOp(opID string) {
	if o.bgops != nil && opID != "" {
		o.bgops.Complete(opID)
	}
}

func (o *Orchestrator) failWorktreeOp(opID string, err error) {
	if o.bgops != nil && opID != "" {
		o.bgops.Fail(opID, err)
	}
}

// CurrentWorktreeHead returns the current HEAD SHA of the git worktree at dir,
// or "" if dir is empty or the git invocation fails.
func CurrentWorktreeHead(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	out, err := gitexec.Output(ctx, gitexec.Options{Dir: dir}, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// FirstNonEmpty returns the first non-empty string among vals, or "" if all
// are empty.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ResolveRoleEffort returns the reasoning-effort level to fall back to for
// role when neither an experiment assignment nor the task itself pins a
// level. It checks the operator-configured Agent.RoleEffort override first
// (validated against task.AllReasoningEfforts), then falls back to the
// role's built-in default (agent.Role.DefaultReasoningEffort). Returns ""
// when nothing applies, letting the manager's global DefaultReasoningEffort
// take over.
func ResolveRoleEffort(role agent.Role, cfg *config.Config) string {
	if cfg != nil {
		if override, ok := cfg.Agent.RoleEffort[string(role)]; ok {
			if _, err := task.ValidateReasoningEffort(override); err == nil {
				return override
			}
		}
	}
	return role.DefaultReasoningEffort()
}
