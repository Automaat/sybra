package sybra

import (
	"context"
	"errors"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

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

// dispatchPass runs the cheap scheduling actions that gate a ready task. Safe to
// run often: maybeStartOrchestrator no-ops when already running, and
// releaseUnblockedChildren only acts on tasks whose dependencies merged.
func (a *App) dispatchPass(ctx context.Context) {
	a.maybeStartOrchestrator(ctx)
	a.releaseUnblockedChildren()
	if a.assigner != nil {
		a.assigner.Tick(ctx)
	}
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
	a.recovery.RestartStaleInProgress(ctx)
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
	a.drainManualQueue(ctx)
	a.reconcileRunnableBoardTasks()
	if a.workflowEngine != nil {
		// workflow.Engine derives its shell-step context from its own e.ctx
		// field (Engine.SetContext, bound once from App's root ctx), not an
		// explicit per-call parameter.
		a.workflowEngine.ResumeStalled() //nolint:contextcheck // Engine uses its own e.ctx field, see comment above
	}
}

func (a *App) reconcileRunnableBoardTasks() {
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
		if t.Workflow != nil {
			continue
		}
		switch t.Status {
		case task.StatusNew, task.StatusTodo, task.StatusPlanning:
			if skipTaskCreatedWorkflow(t) {
				continue
			}
			a.dispatchTaskCreatedWorkflow(t.ID)
		case task.StatusInProgress, task.StatusReadyReview, task.StatusTesting, task.StatusReadyPR:
			a.dispatchStatusWorkflow(t.ID, t.Status)
		default:
		}
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
