package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
	if wctx != nil && !hasEffectiveBlocklist(wctx.Blocklist) {
		return false, fmt.Errorf("route work monitor artifact: work scrub context unavailable")
	}
	if wctx == nil && a.Confidential {
		wctx = s.lookupAnyWorkContext()
		if wctx == nil {
			return false, fmt.Errorf("route confidential monitor artifact: work scrub context unavailable")
		}
	}
	if wctx == nil {
		return s.inner.Submit(ctx, a, body)
	}
	title, _ := scrub.Scrub(monitorArtifactTitle(a), wctx.Blocklist)
	scrubbedBody, redactions := scrub.Scrub(body, wctx.Blocklist)

	if existing, ok := s.findOpen(title, a.Fingerprint); ok {
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
	tags := []string{string(task.FlagSybraBug), string(task.FlagScrubbed), "monitor:" + string(a.Kind)}
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

func (s *monitorRoutingSink) ApplyIncident(ctx context.Context, in monitor.Incident, change monitor.IncidentChange, body string) (bool, monitor.IncidentArtifact, error) {
	a := monitor.Anomaly{Kind: monitor.AnomalyKind(in.FailureCode), Fingerprint: in.Fingerprint, IncidentScope: in.ProjectScope,
		Confidential: in.IsConfidential()}
	if a.Confidential {
		wctx := s.lookupAnyWorkContext()
		if wctx == nil {
			return false, monitor.IncidentArtifact{}, fmt.Errorf("route confidential incident: work scrub context unavailable")
		}
		title, _ := scrub.Scrub(monitor.IncidentTitle(in), wctx.Blocklist)
		if in.State == monitor.IncidentActive {
			if existing, ok := s.findMatching(title, a.Fingerprint, true); ok && task.IsTerminalStatus(existing.Status) {
				scrubbed, _ := scrub.Scrub(body, wctx.Blocklist)
				appended := appendRedetectedNote(existing.Body, scrubbed, s.now())
				if _, err := s.tasks.Apply(task.TransitionIntent{
					TaskID: existing.ID, ToStatus: task.StatusTodo, Actor: "monitor.incident.reopen",
					OperatorOverride: true, Extra: task.Update{Body: &appended},
				}); err != nil {
					return false, monitor.IncidentArtifact{}, err
				}
				s.dispatchCreatedWorkflow(existing.ID)
				return false, monitor.IncidentArtifact{URL: "local:" + in.Fingerprint}, nil
			}
		}
		created, err := s.Submit(ctx, a, body)
		return created, monitor.IncidentArtifact{URL: "local:" + in.Fingerprint}, err
	}
	if sink, ok := s.inner.(monitor.IncidentSink); ok {
		return sink.ApplyIncident(ctx, in, change, body)
	}
	created, err := s.inner.Submit(ctx, a, body)
	return created, monitor.IncidentArtifact{}, err
}

func (s *monitorRoutingSink) ResolveIncident(ctx context.Context, in monitor.Incident, comment string) (bool, error) {
	a := monitor.Anomaly{Kind: monitor.AnomalyKind(in.FailureCode), Fingerprint: in.Fingerprint, IncidentScope: in.ProjectScope,
		Confidential: in.IsConfidential()}
	if a.Confidential {
		return s.CloseIfOpen(ctx, a, comment)
	}
	if sink, ok := s.inner.(monitor.IncidentSink); ok {
		return sink.ResolveIncident(ctx, in, comment)
	}
	return s.CloseIfOpen(ctx, a, comment)
}

func (s *monitorRoutingSink) MapDuplicateIncidents(ctx context.Context, in monitor.Incident, duplicates []int, coverage string) error {
	if in.IsConfidential() {
		return nil
	}
	if sink, ok := s.inner.(monitor.IncidentSink); ok {
		return sink.MapDuplicateIncidents(ctx, in, duplicates, coverage)
	}
	return nil
}

// CloseIfOpen implements monitor.IssueCloser. For a work-typed anomaly it
// marks the matching local investigation task done instead of closing a
// GitHub issue; for everything else it delegates to the wrapped sink (if
// that sink implements IssueCloser too — the production GH sink does).
func (s *monitorRoutingSink) CloseIfOpen(ctx context.Context, a monitor.Anomaly, comment string) (bool, error) {
	title := ""
	wctx := s.lookupWorkContext(a.TaskID)
	if wctx != nil && !hasEffectiveBlocklist(wctx.Blocklist) {
		return false, fmt.Errorf("close work monitor artifact: work scrub context unavailable")
	}
	if wctx == nil && a.Confidential {
		wctx = s.lookupAnyWorkContext()
		if wctx == nil {
			return false, fmt.Errorf("close confidential monitor artifact: work scrub context unavailable")
		}
	}
	if wctx != nil {
		title, _ = scrub.Scrub(monitorArtifactTitle(a), wctx.Blocklist)
	}
	if existing, ok := s.findOpen(title, a.Fingerprint); ok {
		scrubbedComment := comment
		if wctx != nil {
			scrubbedComment, _ = scrub.Scrub(comment, wctx.Blocklist)
		}
		appended := appendRedetectedNote(existing.Body, scrubbedComment, s.now())
		if _, err := s.tasks.Apply(task.TransitionIntent{
			TaskID:   existing.ID,
			ToStatus: task.StatusDone,
			Actor:    "monitor.routing.local_autoclose",
			Extra: task.Update{
				Body: &appended,
			},
		}); err != nil {
			s.logger.Warn("monitor.routing.local.close", "task_id", existing.ID, "err", err)
			return false, err
		}
		s.logger.Info("monitor.routing.local.autoclose", "kind", a.Kind, "src_task_id", a.TaskID, "task_id", existing.ID)
		return true, nil
	}
	closer, ok := s.inner.(monitor.IssueCloser)
	if !ok {
		return false, nil
	}
	return closer.CloseIfOpen(ctx, a, comment)
}

func monitorArtifactTitle(a monitor.Anomaly) string {
	if strings.HasPrefix(a.Fingerprint, "incident:") {
		return monitor.IncidentTitle(monitor.Incident{FailureCode: string(a.Kind), Fingerprint: a.Fingerprint})
	}
	return monitor.IssueTitle(a.Kind, a.Fingerprint)
}

// lookupAnyWorkContext combines every current work-project blocklist. It is
// the fail-closed route for board-wide/mixed incidents with no representative
// TaskID: one project's identifiers must not escape merely because another
// project happened to be chosen first.
func (s *monitorRoutingSink) lookupAnyWorkContext() *WorkScrubContext {
	if s.tasks == nil || s.workCtx == nil {
		return nil
	}
	all, err := s.tasks.List()
	if err != nil {
		s.logger.Warn("monitor.routing.work_context.list", "err", err)
		return nil
	}
	seen := map[string]bool{}
	ctx := &WorkScrubContext{}
	for i := range all {
		wctx := s.workCtx(all[i].ProjectID)
		if wctx == nil {
			continue
		}
		for _, value := range wctx.Blocklist {
			if strings.TrimSpace(value) != "" && !seen[value] {
				ctx.Blocklist = append(ctx.Blocklist, value)
				seen[value] = true
			}
		}
	}
	if !hasEffectiveBlocklist(ctx.Blocklist) {
		return nil
	}
	return ctx
}

func hasEffectiveBlocklist(blocklist []string) bool {
	for _, value := range blocklist {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func (s *monitorRoutingSink) dispatchCreatedWorkflow(taskID string) {
	if s.dispatchCreated == nil {
		return
	}
	s.dispatchCreated(taskID)
}

// findOpen returns the first non-terminal task that matches the monitor chore
// by title or by fingerprint embedded in the body. Title matching covers the
// normal case; fingerprint matching covers tasks that were renamed by the
// triage agent (the body is never modified by triage, so the fingerprint line
// written by DeterministicIssueBody survives a rename). Ok is false when no
// match exists or the task list cannot be read.
func (s *monitorRoutingSink) findOpen(title, fp string) (task.Task, bool) {
	return s.findMatching(title, fp, false)
}

func (s *monitorRoutingSink) findMatching(title, fp string, includeTerminal bool) (task.Task, bool) {
	all, err := s.tasks.List()
	if err != nil {
		s.logger.Warn("monitor.routing.local.list", "err", err)
		return task.Task{}, false
	}
	marker := "- Fingerprint: `" + fp + "`"
	for i := range all {
		t := &all[i]
		if !includeTerminal && task.IsTerminalStatus(t.Status) {
			continue
		}
		if t.Title == title || strings.Contains(t.Body, marker) {
			return *t, true
		}
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
	if s.tasks == nil || s.workCtx == nil || taskID == "" {
		return nil
	}
	t, err := s.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return nil
	}
	return s.workCtx(t.ProjectID)
}
