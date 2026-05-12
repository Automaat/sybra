package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
//
// Dedup: an open local task whose title matches the anomaly's IssueTitle is
// reused — the scrubbed body is appended as a "re-detected" note and Submit
// returns (false, nil), mirroring the GH sink's comment-on-hit contract.
// Terminal tasks (done, cancelled) are ignored so a recurrence after closure
// opens a fresh task.
type monitorRoutingSink struct {
	inner           monitor.IssueSink
	tasks           *task.Manager
	workCtx         func(projectID string) *WorkScrubContext
	projectID       string
	dispatchCreated func(taskID string)
	logger          *slog.Logger
	now             func() time.Time
}

func newMonitorRoutingSink(
	inner monitor.IssueSink,
	tasks *task.Manager,
	workCtx func(string) *WorkScrubContext,
	projectID string,
	dispatchCreated func(taskID string),
	logger *slog.Logger,
) *monitorRoutingSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &monitorRoutingSink{
		inner:           inner,
		tasks:           tasks,
		workCtx:         workCtx,
		projectID:       projectID,
		dispatchCreated: dispatchCreated,
		logger:          logger,
		now:             time.Now,
	}
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

	if existing, ok := s.findOpenByTitle(title); ok {
		appended := appendRedetectedNote(existing.Body, scrubbedBody, s.now())
		if _, err := s.tasks.Update(existing.ID, task.Update{Body: &appended}); err != nil {
			s.logger.Warn("monitor.routing.local.append", "task_id", existing.ID, "err", err)
			return false, err
		}
		s.logger.Info("monitor.routing.local.dedup",
			"kind", a.Kind, "src_task_id", a.TaskID,
			"task_id", existing.ID, "redactions", redactions)
		return false, nil
	}

	newTask, err := s.tasks.Create(title, scrubbedBody, task.AgentModeHeadless)
	if err != nil {
		s.logger.Error("monitor.routing.local.create", "src_task_id", a.TaskID, "kind", a.Kind, "err", err)
		return false, err
	}
	tags := []string{"sybra-bug", "scrubbed", "monitor:" + string(a.Kind)}
	update := task.Update{Tags: &tags}
	if s.projectID != "" {
		pid := s.projectID
		update.ProjectID = &pid
	}
	if _, err := s.tasks.Update(newTask.ID, update); err != nil {
		s.logger.Warn("monitor.routing.local.tag", "new_task_id", newTask.ID, "err", err)
	}
	s.dispatchCreatedWorkflow(newTask.ID)
	s.logger.Info("monitor.routing.local",
		"kind", a.Kind, "src_task_id", a.TaskID,
		"new_task_id", newTask.ID, "project_id", s.projectID, "redactions", redactions)
	return true, nil
}

func (s *monitorRoutingSink) dispatchCreatedWorkflow(taskID string) {
	if s.dispatchCreated == nil {
		return
	}
	s.dispatchCreated(taskID)
}

// findOpenByTitle returns the first non-terminal task whose Title equals the
// fingerprint-stable issue title. Ok is false when no match exists or the
// task list cannot be read.
func (s *monitorRoutingSink) findOpenByTitle(title string) (task.Task, bool) {
	all, err := s.tasks.List()
	if err != nil {
		s.logger.Warn("monitor.routing.local.list", "err", err)
		return task.Task{}, false
	}
	for i := range all {
		t := &all[i]
		if t.Title != title {
			continue
		}
		if task.IsTerminalStatus(t.Status) {
			continue
		}
		return *t, true
	}
	return task.Task{}, false
}

// appendRedetectedNote appends a timestamped "re-detected" block carrying the
// latest scrubbed body so each cycle's evidence is preserved without spawning
// a new task. Idempotent enough: every cycle gets one block; the source task
// id stays in the original title.
func appendRedetectedNote(existing, body string, now time.Time) string {
	stamp := now.UTC().Format(time.RFC3339)
	note := fmt.Sprintf("\n\n## Re-detected at %s\n%s\n", stamp, body)
	return existing + note
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
