package sybra

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

const inboundReviewRedispatchCooldown = 10 * time.Minute

// orchestratorLoop runs two cadences. The cheap, latency-sensitive dispatch pass
// (start the orchestrator, release unblocked children) fires on a fast ticker and
// on demand via dispatchNudge, so a freshly-ready task isn't left idle. The
// expensive recovery/cleanup pass (resume stalled workflows, restart stale
// agents, prune orphan worktrees) — which hits git and may spawn agents — fires
// on a slower ticker so it never runs hot.
func (a *App) orchestratorLoop(ctx context.Context) {
	a.queueDrainPass(ctx)

	dispatch := time.NewTicker(a.dispatchInterval())
	defer dispatch.Stop()
	maintenance := time.NewTicker(a.maintenanceInterval())
	defer maintenance.Stop()
	var queueNudge <-chan struct{}
	if a.agents != nil {
		queueNudge = a.agents.QueueNudge()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-queueNudge:
			a.queueDrainPass(ctx)
		case <-a.dispatchNudge:
			a.dispatchPass(ctx)
		case <-dispatch.C:
			a.dispatchPass(ctx)
		case <-maintenance.C:
			a.maintenancePass(ctx)
		}
	}
}

// dispatchPass runs the cheap scheduling actions that gate a ready task and
// reconciles idle board tasks that should already be moving. Safe to run often:
// maybeStartOrchestrator no-ops when already running, releaseUnblockedChildren
// only acts on tasks whose dependencies merged, and workflow dispatch helpers
// refuse active workflows/running agents.
func (a *App) dispatchPass(ctx context.Context) {
	a.maybeStartOrchestrator(ctx)
	if !a.runsScheduler() {
		return
	}
	a.releaseUnblockedChildren()
	a.reconcileRunnableBoardTasks(ctx)
	if a.assigner != nil {
		a.assigner.Tick(ctx)
	}
}

// applyInstanceRole resolves the instance-role gates once. They are sampled
// rather than read per tick because config reload rewrites cfg.Orchestrator in
// place under the ConfigService lock, which would race the orchestrator loop;
// role changes are restart-required, matching orchestrator.intervals. Also
// surfaces an invalid role — internal/config cannot log it itself, since
// slog's default logger is not the server's logger and the warning would land
// below the shipped level.
func (a *App) applyInstanceRole() {
	scheduler, brain := true, true
	if a.cfg != nil {
		if _, err := config.NormalizeInstanceRole(a.cfg.Orchestrator.Role); err != nil && a.logger != nil {
			a.logger.Warn("config.orchestrator.role.invalid",
				"value", a.cfg.Orchestrator.Role, "fallback", config.InstanceRoleFull)
		}
		scheduler = a.cfg.Orchestrator.RunsScheduler()
		brain = a.cfg.Orchestrator.RunsOrchestrator()
	}
	a.schedulerDisabled.Store(!scheduler)
	a.brainDisabled.Store(!brain)
	// The engine is the authoritative gate: it has callers across TaskService,
	// review, completion, PR integrations, promptlab and the watcher, and gating
	// those individually leaked three times.
	if a.workflowEngine != nil {
		a.workflowEngine.SetAutoDispatch(scheduler)
	}
}

// runsScheduler reports whether this instance may auto-dispatch work. An
// agent-only instance still serves the HTTP API and runs explicitly-started
// agents; it just never schedules any itself.
func (a *App) runsScheduler() bool {
	return !a.schedulerDisabled.Load()
}

// runsOrchestratorBrain reports whether this instance may auto-start the
// orchestrator session. Only gates the automatic start — an operator's manual
// StartOrchestrator call stays available on every instance.
func (a *App) runsOrchestratorBrain() bool {
	return !a.brainDisabled.Load()
}

const clusterHealthProbeInterval = 30 * time.Second

func (a *App) clusterHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(clusterHealthProbeInterval)
	defer ticker.Stop()
	a.clusterRoster.ProbeAll(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.clusterRoster.ProbeAll(ctx, time.Now())
		}
	}
}

// maintenancePass runs the expensive, git/agent-touching recovery and cleanup.
func (a *App) maintenancePass(ctx context.Context) {
	metrics.OrchestratorTick(ctx)
	a.queueDrainPass(ctx)
	// Recover in-progress tasks whose agent died — runs continuously, not just at
	// startup, to catch agents that finished without advancing the workflow.
	if a.runsScheduler() {
		a.recovery.RestartStaleInProgress(ctx)
	}
	a.recovery.ReconcileLostPRNumber(ctx)
	// Re-attempt enrichment for URL stubs orphaned by a failed/interrupted
	// initial fetch — otherwise they keep the enrich-pending marker (and their
	// raw-URL title) forever and never dispatch a workflow. The eventual
	// agent.Manager dispatch chain uses its own m.ctx field, same pattern.
	if a.taskSvc != nil {
		a.taskSvc.ReconcilePendingEnrichment() //nolint:contextcheck // agent.Manager dispatch chain uses its own m.ctx field, see comment above
	}
	a.worktrees.CleanupOrphaned(ctx)
	if a.sandboxes != nil && a.tasks != nil {
		if tasks, err := a.tasks.List(); err == nil {
			var hasAgent func(string) bool
			if a.agents != nil {
				hasAgent = a.agents.HasRunningAgentForTask
			}
			a.sandboxes.CleanupOrphaned(ctx, tasks, hasAgent)
		}
	}
}

func (a *App) queueDrainPass(ctx context.Context) {
	// Draining the manual queue is the resume path for an agent an operator
	// already explicitly started, not auto-dispatch — an agent-only instance
	// must still finish it, or a start that landed on a busy pool is stranded
	// forever (nothing else pops manual items).
	a.drainManualQueue(ctx)
	if !a.runsScheduler() {
		return
	}
	a.reconcileRunnableBoardTasks(ctx)
	if a.workflowEngine != nil {
		// workflow.Engine derives its shell-step context from its own e.ctx
		// field (Engine.SetContext, bound once from App's root ctx), not an
		// explicit per-call parameter.
		a.workflowEngine.ResumeStalled() //nolint:contextcheck // Engine uses its own e.ctx field, see comment above
	}
}

func (a *App) reconcileRunnableBoardTasks(ctx context.Context) {
	if a.workflowEngine == nil || a.tasks == nil || a.agents == nil {
		return
	}
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Warn("workflow.reconcile-runnable.list", "err", err)
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.TaskType == task.TaskTypeChat || t.TaskType == task.TaskTypeUmbrella {
			continue
		}
		if !a.runsTaskLocally(t) {
			continue
		}
		if a.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		if boardReconcileWorkflowActive(t) {
			continue
		}
		switch t.Status {
		case task.StatusNew, task.StatusTodo:
			if skipTaskCreatedWorkflow(t) {
				continue
			}
			a.dispatchTaskCreatedWorkflow(t.ID)
		case task.StatusPlanning:
			a.dispatchPlanningWorkflow(t.ID)
		case task.StatusInReview:
			if isInboundReviewTask(t) {
				a.dispatchInboundReviewWorkflow(ctx, t.ID)
				continue
			}
		case task.StatusInProgress, task.StatusReadyReview, task.StatusTesting, task.StatusReadyPR:
			a.dispatchStatusWorkflow(t.ID, t.Status)
		default:
		}
	}
}

func isInboundReviewTask(t task.Task) bool {
	return slices.Contains(t.Tags, "review") && t.ProjectID != "" && t.PRNumber != 0
}

// maxReviewAttemptsPerHead bounds automated review runs against one PR commit.
// Above 1 so a transient provider failure gets a retry: refusing after a single
// dispatch would trade #2164's re-review loop for a PR nobody ever reviews
// again, which is the worse outcome for a contributor.
const maxReviewAttemptsPerHead = 2

// reviewCoversHead reports whether head's review budget is spent. An unknown
// head ("") counts as covered: we cannot tell a new push from the commit we
// just reviewed, and declining is the safe direction.
func reviewCoversHead(t task.Task, head string) bool {
	if head == "" {
		return true
	}
	if t.ReviewedHeadSHA != head {
		return false
	}
	return t.ReviewedHeadAttempts >= maxReviewAttemptsPerHead
}

// nextReviewAttempt returns the attempt number head is about to consume. A new
// head restarts the budget, so a real push always re-opens review.
func nextReviewAttempt(t task.Task, head string) int {
	if t.ReviewedHeadSHA != head {
		return 1
	}
	return t.ReviewedHeadAttempts + 1
}

// reviewRoundWindow is the rolling window reviewRoundsSpent counts over.
const reviewRoundWindow = time.Hour

// reviewRoundsSpent counts automated review runs started on t within the last
// window, read off the durable AgentRuns list so it survives a restart.
func reviewRoundsSpent(t task.Task, now time.Time) int {
	cutoff := now.Add(-reviewRoundWindow)
	spent := 0
	for i := range t.AgentRuns {
		if t.AgentRuns[i].Role != string(agent.RoleReview) {
			continue
		}
		if t.AgentRuns[i].StartedAt.After(cutoff) {
			spent++
		}
	}
	return spent
}

// reviewRateLimitExceeded reports whether t has spent its review budget for the
// current window. limit <= 0 disables the cap.
func reviewRateLimitExceeded(t task.Task, now time.Time, limit int) bool {
	if limit <= 0 {
		return false
	}
	return reviewRoundsSpent(t, now) >= limit
}

// reviewRoundsPerHourLimit resolves the cap; a nil cfg (tests) uses the default.
func (a *App) reviewRoundsPerHourLimit() int {
	if a.cfg == nil {
		return config.DefaultReviewRoundsPerHour
	}
	return a.cfg.GitHub.ReviewRoundsPerHourLimit()
}

// parkReviewRateLimited trips the breaker: a task posting reviews this fast is
// misbehaving, and a tripped breaker needs a human to reset rather than
// self-healing into the next burst.
func (a *App) parkReviewRateLimited(t task.Task, limit int) {
	a.logger.Error("workflow.dispatch.inbound-review.rate-limit",
		"task_id", t.ID, "repo", t.ProjectID, "pr", t.PRNumber,
		"rounds", reviewRoundsSpent(t, time.Now()), "limit", limit)
	reason := fmt.Sprintf("automated review rate limit: %d rounds within an hour on PR #%d", limit, t.PRNumber)
	if _, err := a.tasks.Update(t.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		a.logger.Error("workflow.dispatch.inbound-review.rate-limit-park", "task_id", t.ID, "err", err)
	}
}

// fetchPRHeadSHAFunc returns the PR-head lookup, overridable in tests. The
// context-aware form is mandatory here: this runs inline on the orchestrator
// loop, and gh's global request gate is held across the whole subprocess, so an
// uncancellable call would stall every other dispatch behind it.
func (a *App) fetchPRHeadSHAFunc() func(ctx context.Context, repo string, number int) (string, error) {
	if a.fetchPRHeadSHA != nil {
		return a.fetchPRHeadSHA
	}
	return github.FetchPRHeadSHAContext
}

func (a *App) dispatchInboundReviewWorkflow(ctx context.Context, taskID string) {
	if a.workflowEngine == nil || a.tasks == nil || a.agents == nil {
		return
	}
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	if t.Status != task.StatusInReview || !isInboundReviewTask(t) {
		return
	}
	if !inboundReviewNeedsAgent(t) {
		return
	}
	if !a.runsTaskLocally(t) {
		return
	}
	if t.Workflow != nil && t.Workflow.State != workflow.ExecCompleted && t.Workflow.State != workflow.ExecFailed {
		return
	}
	if a.agents.HasRunningAgentForTask(taskID) {
		return
	}

	// The blast-radius cap, and the only gate here that bounds a loop we have
	// not thought of: the per-head budget above assumes the head is a
	// meaningful key, and every other gate assumes the phase machine is sane.
	// Rate rather than lifetime total, so a PR legitimately re-reviewed after
	// each push over weeks is never blocked while a runaway is stopped inside
	// the hour. Counted off the durable AgentRuns list, so a restart cannot
	// launder it. Checked before the GitHub call — it needs no network.
	if limit := a.reviewRoundsPerHourLimit(); reviewRateLimitExceeded(t, time.Now(), limit) {
		a.parkReviewRateLimited(t, limit)
		return
	}

	// Every gate above is a function of local state, so a stale or frozen phase
	// re-opens them every cooldown — that is how #2164 spent 112 reviews on one
	// commit. Ask GitHub for the real head and spend a bounded budget per
	// commit. Cached 30s (github.prHeadSHACache), so a task whose gates are
	// stuck open costs ~2 lookups/min, not one per tick.
	headCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	head, err := a.fetchPRHeadSHAFunc()(headCtx, t.ProjectID, t.PRNumber)
	if err != nil {
		// Fail closed: without the head we cannot tell a new push from the
		// commit we already reviewed, and guessing is what produced 112 reviews.
		a.logger.Warn("workflow.dispatch.inbound-review.head", "task_id", taskID, "err", err)
		return
	}
	if head == "" {
		// Declining is right, but silence is not: an empty SHA with no error means
		// gh returned something we don't understand, and a PR that is quietly
		// never reviewed again is exactly what #2164 was hard to diagnose about.
		a.logger.Warn("workflow.dispatch.inbound-review.head-empty",
			"task_id", taskID, "repo", t.ProjectID, "pr", t.PRNumber)
		return
	}
	if reviewCoversHead(t, head) {
		return
	}

	if err := a.workflowEngine.StartWorkflow(taskID, "pr-review"); err != nil {
		if !errors.Is(err, workflow.ErrWorkflowAlreadyActive) &&
			!errors.Is(err, workflow.ErrAutoDispatchDisabled) {
			a.logger.Error("workflow.dispatch.inbound-review", "task_id", taskID, "err", err)
		}
		// Nothing ran, so nothing is charged; the next tick stays free to retry.
		return
	}
	// Charge only once a run is underway. A run that then crashes or loops still
	// burns its attempt, which is what bounds the loop.
	attempt := nextReviewAttempt(t, head)
	if _, err := a.tasks.Update(taskID, task.Update{
		ReviewedHeadSHA:      task.Ptr(head),
		ReviewedHeadAttempts: task.Ptr(attempt),
	}); err != nil {
		a.logger.Error("workflow.dispatch.inbound-review.stamp", "task_id", taskID, "err", err)
	}
}

func (a *App) dispatchPlanningWorkflow(taskID string) {
	if !a.runsScheduler() {
		return
	}
	if a.workflowEngine == nil || a.tasks == nil || a.agents == nil {
		return
	}
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	if t.Status != task.StatusPlanning {
		return
	}
	if !a.runsTaskLocally(t) {
		return
	}
	if skipTaskCreatedWorkflow(t) {
		return
	}
	if t.Workflow != nil && t.Workflow.State != workflow.ExecCompleted && t.Workflow.State != workflow.ExecFailed {
		return
	}
	if a.agents.HasRunningAgentForTask(taskID) {
		return
	}
	if err := a.workflowEngine.StartWorkflow(taskID, "simple-task-plan"); err != nil &&
		!errors.Is(err, workflow.ErrWorkflowAlreadyActive) &&
		!errors.Is(err, workflow.ErrAutoDispatchDisabled) {
		a.logger.Error("workflow.dispatch.planning", "task_id", taskID, "err", err)
	}
}

func boardReconcileWorkflowActive(t task.Task) bool {
	if t.Workflow == nil {
		return false
	}
	switch t.Workflow.State {
	case workflow.ExecCompleted, workflow.ExecFailed:
		switch t.Status {
		case task.StatusInProgress, task.StatusReadyReview, task.StatusTesting, task.StatusReadyPR:
			return false
		case task.StatusInReview:
			if isInboundReviewTask(t) && inboundReviewNeedsAgent(t) {
				return false
			}
		default:
		}
		return t.Workflow.CompletedAt == nil ||
			t.StatusChangedAt.IsZero() ||
			!t.StatusChangedAt.After(*t.Workflow.CompletedAt)
	default:
		return true
	}
}

func inboundReviewNeedsAgent(t task.Task) bool {
	switch t.ReviewPhase {
	case "":
		// ReviewPhase only moves off "" once reconcileReviewPhases observes a
		// live GitHub signal (submitted review, draft, conflict, ...) on its
		// own poll cadence — which can lag well behind a review agent
		// finishing, or never fire at all offline. Once a pr-review workflow
		// has actually completed, the review already ran; treat that as
		// "reviewed" instead of re-dispatching on every tick until the poll
		// catches up. A task with no pr-review workflow yet (nil, or stranded
		// on a foreign workflow like simple-task-plan from the
		// create-before-tag race) still genuinely needs its first dispatch.
		return t.Workflow == nil || t.Workflow.WorkflowID != "pr-review"
	case "needs-approval":
		if t.Workflow == nil || t.Workflow.CompletedAt == nil {
			return true
		}
		return time.Since(*t.Workflow.CompletedAt) >= inboundReviewRedispatchCooldown
	default:
		return false
	}
}

func (a *App) drainManualQueue(ctx context.Context) {
	if a.agentQueue == nil || a.agentOrch == nil || a.agents == nil || a.tasks == nil {
		return
	}
	a.agentQueue.Reconcile(func(id string) (task.Task, bool) {
		t, err := a.tasks.Get(id)
		return t, err == nil
	})
	snap := a.agentQueue.Snapshot()
	manualDepth := 0
	for i := range snap {
		if snap[i].Manual {
			manualDepth++
		}
	}
	slots := a.agents.AvailableQueueDrainSlots(manualDepth)
	if slots <= 0 {
		return
	}
	popped := a.agentQueue.PopManualReady(slots)
	for i := range popped {
		it := popped[i]
		ag, err := a.agentOrch.StartQueuedManualItem(ctx, it)
		if ag != nil && ag.GetState() == agent.StateQueued {
			a.restoreManualQueueItem(it)
			continue
		}
		if err == nil {
			continue
		}
		if errors.Is(err, workflow.ErrAgentPoolBusy) || errors.Is(err, workflow.ErrDispatchInFlight) {
			a.restoreManualQueueItem(it)
			continue
		}
		a.logger.Warn("agentqueue.manual-drain.dispatch", "task_id", it.TaskID, "err", err)
	}
}

func (a *App) restoreManualQueueItem(it agentqueue.Item) {
	if a.agentQueue == nil {
		return
	}
	if restored := a.agentQueue.Restore(it); restored {
		return
	}
	snap := a.agentQueue.Snapshot()
	for i := range snap {
		if snap[i].TaskID == it.TaskID && snap[i].Manual {
			return
		}
	}
	a.logger.Warn("agentqueue.manual-drain.restore", "task_id", it.TaskID)
}

// nudgeDispatch asks the orchestrator loop to run a dispatch pass promptly
// instead of waiting for the next fast tick. Non-blocking and coalescing: a full
// buffer means a pass is already pending. If the loop hasn't started yet, the
// buffered signal is drained on its first iteration; the nil-guard only skips
// when the channel was never created (non-NewApp test construction).
func (a *App) nudgeDispatch() {
	if a.dispatchNudge == nil {
		return
	}
	select {
	case a.dispatchNudge <- struct{}{}:
	default:
	}
}

func (a *App) dispatchInterval() time.Duration {
	s := a.cfg.Orchestrator.DispatchIntervalSeconds
	if s <= 0 {
		s = 10
	}
	return time.Duration(s) * time.Second
}

func (a *App) maintenanceInterval() time.Duration {
	s := a.cfg.Orchestrator.MaintenanceIntervalSeconds
	if s <= 0 {
		s = 60
	}
	return time.Duration(s) * time.Second
}

func (a *App) maybeStartOrchestrator(ctx context.Context) {
	if !a.runsOrchestratorBrain() {
		return
	}
	if a.orchSvc.IsOrchestratorRunning() {
		return
	}

	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	hasActive := false
	for i := range tasks {
		if tasks[i].TaskType == task.TaskTypeChat {
			continue
		}
		switch tasks[i].Status {
		case task.StatusPlanning, task.StatusPlanReview, task.StatusInProgress, task.StatusInReview:
			hasActive = true
		default:
		}
		if hasActive {
			break
		}
	}
	if !hasActive {
		return
	}

	a.logger.Info("orchestrator.auto-start", "reason", "active tasks detected")
	if err := a.orchSvc.StartOrchestratorContext(ctx); err != nil {
		a.logger.Error("orchestrator.auto-start.failed", "err", err)
	}
}
