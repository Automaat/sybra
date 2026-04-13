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

// recoverMonitorCron is invoked by MonitorWatchdog when the heartbeat
// file goes stale. The /synapse-monitor cron is bootstrapped from the
// orchestrator session's first-turn CronCreate call, so the cheapest way
// to re-create it is to restart the orchestrator: stop the current session
// (if any) and start a fresh one. The watchdog's internal cooldown gates
// how often this runs, so we do not need a second guard here.
func (a *App) recoverMonitorCron() {
	if a.orchSvc == nil {
		return
	}
	if a.orchSvc.IsOrchestratorRunning() {
		a.logger.Warn("monitor.watchdog.restart-orchestrator")
		if err := a.orchSvc.StopOrchestrator(); err != nil {
			a.logger.Error("monitor.watchdog.stop-failed", "err", err)
			return
		}
	}
	if err := a.orchSvc.StartOrchestrator(); err != nil {
		a.logger.Error("monitor.watchdog.start-failed", "err", err)
	}
}

// GetMonitorHeartbeat returns the current watchdog snapshot for the
// Orchestrator frontend page. Safe to call before Startup finishes: a
// zero-valued status is returned until the watchdog runs its first tick.
func (a *App) GetMonitorHeartbeat() MonitorStatus {
	if a.monitorWatch == nil {
		return MonitorStatus{}
	}
	return a.monitorWatch.Status()
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
