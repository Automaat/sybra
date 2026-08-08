package sybra

import (
	"context"
	"errors"

	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

func (a *App) wireTaskService() {
	a.taskSvc.ctx = a.ctx
	a.taskSvc.tasks = a.tasks
	a.taskSvc.agents = a.agents
	a.taskSvc.workflowEngine = a.workflowEngine
	a.taskSvc.worktrees = a.worktrees
	a.taskSvc.sandboxes = a.sandboxes
	a.taskSvc.artifacts = a.artifacts
	a.taskSvc.attachments = a.attachments
	a.taskSvc.wg = &a.wg
	a.taskSvc.logger = a.logger
	a.taskSvc.audit = a.audit
	a.taskSvc.cfg = a.currentConfig()
	a.taskSvc.currentConfig = a.currentConfig
	a.taskSvc.projects = a.projects
	a.taskSvc.intervention = a.intervention
	a.taskSvc.abTesting = a.abTestingConfig
	a.taskSvc.assigner = a.assigner
	a.taskSvc.monitorScan = func(ctx context.Context) (monitor.Report, error) {
		if a.monitorSvc == nil {
			return monitor.Report{}, errors.New("monitor is not running on this instance")
		}
		return a.monitorSvc.Scan(ctx)
	}
	a.taskSvc.recoverLostAgent = func(ctx context.Context, taskID string) error {
		if a.recovery == nil {
			return nil
		}
		return a.recovery.RestartTaskIfStale(ctx, taskID)
	}

	// Expand a manually-added umbrella issue into a gated child DAG instead of
	// a flat task. Wired unconditionally; enrichFromIssue gates the call on
	// cfg.Umbrella.Enabled so a config reload toggles it without re-wiring.
	// Read one current snapshot inside the closure so config reloads update the
	// planner model without mixing settings from different snapshots.
	// Mirrors initIssuesFetcher's poll-loop expander (same Expand entry point).
	// An empty model means the caller expressed no preference; `sybra-cli
	// umbrella --model` passes one, and dropping it would expand with the
	// server's default and report success as if the flag had applied.
	a.taskSvc.umbrellaExpand = func(issueURL, model string) (umbrella.Result, error) {
		cfg := a.currentConfig()
		var opts []umbrella.ExpandOption
		if cfg.Umbrella.Ground {
			opts = append(opts, umbrella.WithExpandGrounder(buildGroundLister(a.projects), cfg.Umbrella.GroundMinSubIssues))
		}
		if model == "" {
			model = cfg.Umbrella.Model
		}
		return umbrella.Expand(a.ctx, a.tasks, umbrella.FallbackPlannerRunner(model, a.providerHealth), issueURL, opts...)
	}
	if a.humanReview != nil {
		a.humanReview.dispatchFromHumanRequired = func(id, target, reason, completingAgentID string) (task.Task, error) {
			return a.taskSvc.dispatchFromHumanRequiredAllowingAgent(id, target, reason, completingAgentID)
		}
		a.humanReview.landClosedPR = func(ctx context.Context, taskID string, prNumber int, state, completingAgentID string) error {
			if a.reviewer == nil {
				return errors.New("review handler unavailable")
			}
			return a.reviewer.AdvanceClosedTaskPRAllowingAgent(ctx, taskID, prNumber, state, completingAgentID)
		}
	}
}
