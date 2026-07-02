package sybra

import (
	"context"
	"time"

	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/task"
)

// orchestratorLoop runs two cadences. The cheap, latency-sensitive dispatch pass
// (start the orchestrator, release unblocked children) fires on a fast ticker and
// on demand via dispatchNudge, so a freshly-ready task isn't left idle. The
// expensive recovery/cleanup pass (resume stalled workflows, restart stale
// agents, prune orphan worktrees) — which hits git and may spawn agents — fires
// on a slower ticker so it never runs hot.
func (a *App) orchestratorLoop(ctx context.Context) {
	dispatch := time.NewTicker(a.dispatchInterval())
	defer dispatch.Stop()
	maintenance := time.NewTicker(a.maintenanceInterval())
	defer maintenance.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.dispatchNudge:
			// dispatchPass ultimately reaches agent.Manager.Run (via
			// maybeStartOrchestrator -> OrchestratorService.StartOrchestrator),
			// a Wails-bound method the frontend also invokes directly with no
			// arguments, and whose signature is shared by dozens of unrelated
			// dispatch call sites. Threading ctx through it would be a broad,
			// unrelated API change rather than a targeted cancellation fix.
			a.dispatchPass() //nolint:contextcheck // Wails-bound StartOrchestrator + Manager.Run boundary, see comment above
		case <-dispatch.C:
			a.dispatchPass() //nolint:contextcheck // Wails-bound StartOrchestrator + Manager.Run boundary, see comment above
		case <-maintenance.C:
			a.maintenancePass(ctx)
		}
	}
}

// dispatchPass runs the cheap scheduling actions that gate a ready task. Safe to
// run often: maybeStartOrchestrator no-ops when already running, and
// releaseUnblockedChildren only acts on tasks whose dependencies merged.
func (a *App) dispatchPass() {
	a.maybeStartOrchestrator()
	a.releaseUnblockedChildren()
}

// maintenancePass runs the expensive, git/agent-touching recovery and cleanup.
func (a *App) maintenancePass(ctx context.Context) {
	metrics.OrchestratorTick(ctx)
	if a.workflowEngine != nil {
		// workflow.Engine derives its shell-step context from its own e.ctx
		// field (Engine.SetContext, bound once from App's root ctx), not an
		// explicit per-call parameter.
		a.workflowEngine.ResumeStalled() //nolint:contextcheck // Engine uses its own e.ctx field, see comment above
	}
	// Recover in-progress tasks whose agent died — runs continuously, not just at
	// startup, to catch agents that finished without advancing the workflow.
	a.recovery.RestartStaleInProgress(ctx)
	// Re-attempt enrichment for URL stubs orphaned by a failed/interrupted
	// initial fetch — otherwise they keep the enrich-pending marker (and their
	// raw-URL title) forever and never dispatch a workflow. The eventual
	// agent.Manager dispatch chain uses its own m.ctx field, same pattern.
	if a.taskSvc != nil {
		a.taskSvc.ReconcilePendingEnrichment() //nolint:contextcheck // agent.Manager dispatch chain uses its own m.ctx field, see comment above
	}
	a.worktrees.CleanupOrphaned(ctx)
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

func (a *App) maybeStartOrchestrator() {
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
	if err := a.orchSvc.StartOrchestrator(); err != nil {
		a.logger.Error("orchestrator.auto-start.failed", "err", err)
	}
}
