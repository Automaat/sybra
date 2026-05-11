package sybra

import (
	"context"
	"log/slog"

	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/task"
)

// monitorRoutingSink wraps a monitor.IssueSink so anomalies on work-typed
// tasks never reach the public sybra repo. When workCtx returns non-nil for
// an anomaly's project, the sink scrubs the deterministic body through the
// project's blocklist and creates a local sybra task tagged sybra-bug,
// scrubbed instead of delegating to the wrapped sink.
//
// Non-work anomalies (and board-wide anomalies with no TaskID) pass through
// to the inner sink unchanged.
type monitorRoutingSink struct {
	inner   monitor.IssueSink
	tasks   *task.Manager
	workCtx func(projectID string) *WorkScrubContext
	logger  *slog.Logger
}

func newMonitorRoutingSink(inner monitor.IssueSink, tasks *task.Manager, workCtx func(string) *WorkScrubContext, logger *slog.Logger) *monitorRoutingSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &monitorRoutingSink{inner: inner, tasks: tasks, workCtx: workCtx, logger: logger}
}

// Submit implements monitor.IssueSink. Routes to a local scrubbed sybra task
// for work-typed anomalies, otherwise delegates to the wrapped GH sink.
func (s *monitorRoutingSink) Submit(ctx context.Context, a monitor.Anomaly, body string) (bool, error) {
	wctx := s.lookupWorkContext(a.TaskID)
	if wctx == nil {
		return s.inner.Submit(ctx, a, body)
	}
	title, _ := scrub.Scrub(monitor.IssueTitle(a.Kind, a.Fingerprint), wctx.Blocklist)
	scrubbedBody, redactions := scrub.Scrub(body, wctx.Blocklist)
	newTask, err := s.tasks.Create(title, scrubbedBody, task.AgentModeHeadless)
	if err != nil {
		s.logger.Error("monitor.routing.local.create", "src_task_id", a.TaskID, "kind", a.Kind, "err", err)
		return false, err
	}
	tags := []string{"sybra-bug", "scrubbed", "monitor:" + string(a.Kind)}
	if _, err := s.tasks.Update(newTask.ID, task.Update{Tags: &tags}); err != nil {
		s.logger.Warn("monitor.routing.local.tag", "new_task_id", newTask.ID, "err", err)
	}
	s.logger.Info("monitor.routing.local",
		"kind", a.Kind, "src_task_id", a.TaskID,
		"new_task_id", newTask.ID, "redactions", redactions)
	return true, nil
}

// lookupWorkContext returns a WorkScrubContext if the anomaly's task belongs
// to a work-typed project. Empty TaskID, missing task, missing project_id,
// or non-work project all yield nil — anomaly should pass through to the
// wrapped sink.
func (s *monitorRoutingSink) lookupWorkContext(taskID string) *WorkScrubContext {
	if s.workCtx == nil || taskID == "" {
		return nil
	}
	t, err := s.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return nil
	}
	return s.workCtx(t.ProjectID)
}
