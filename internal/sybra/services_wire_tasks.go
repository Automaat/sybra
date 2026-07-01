package sybra

import (
	"context"

	"github.com/Automaat/sybra/internal/umbrella"
)

func (a *App) wireTaskService() {
	a.taskSvc.tasks = a.tasks
	a.taskSvc.agents = a.agents
	a.taskSvc.workflowEngine = a.workflowEngine
	a.taskSvc.worktrees = a.worktrees
	a.taskSvc.sandboxes = a.sandboxes
	a.taskSvc.artifacts = a.artifacts
	a.taskSvc.wg = &a.wg
	a.taskSvc.logger = a.logger
	a.taskSvc.audit = a.audit
	a.taskSvc.cfg = a.cfg

	// Expand a manually-added umbrella issue into a gated child DAG instead of
	// a flat task. Wired unconditionally; enrichFromIssue gates the call on
	// cfg.Umbrella.Enabled so a config reload toggles it without re-wiring.
	// Read a.cfg inside the closure so config reloads update the planner model.
	// Mirrors initIssuesFetcher's poll-loop expander (same Expand entry point).
	a.taskSvc.umbrellaExpand = func(issueURL string) (umbrella.Result, error) {
		return umbrella.Expand(context.Background(), a.tasks, umbrella.FallbackPlannerRunner(a.cfg.Umbrella.Model, a.providerHealth), issueURL)
	}
}
