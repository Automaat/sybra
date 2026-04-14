package synapse

import (
	"context"
	"time"

	"github.com/Automaat/synapse/internal/metrics"
	"github.com/Automaat/synapse/internal/task"
)

func (a *App) orchestratorLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics.OrchestratorTick()
			a.maybeStartOrchestrator()
			if a.workflowEngine != nil {
				a.workflowEngine.ResumeStalled()
			}
			// Recover in-progress tasks whose agent died — runs continuously,
			// not just at startup, to catch agents that finished without
			// advancing the workflow.
			a.restartStaleInProgress()
			a.worktrees.CleanupOrphaned()
		}
	}
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
