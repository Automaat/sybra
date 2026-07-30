package intervention

import (
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/task"
)

// Capture is the single guard+scrub+persist+audit pipeline every real exit
// path from human-required must call. Centralizing it here (instead of one
// copy per caller package) is deliberate: sybra#2468's plan review rejected
// an earlier design that hooked only the operator-initiated manual dispatch
// path (internal/sybra/svc_tasks.go), leaving two automated exit paths in
// internal/sybra/review (reconcileHumanRequiredBlockers,
// advanceClosedTaskPR) unhooked — silently making the auto_recovery
// classification unreachable and the "record all unblocks" acceptance
// criterion false. Every caller (across both packages) now goes through this
// one function, so a future new exit path only needs to call Capture, not
// re-derive the guard/scrub/audit chain.
//
// Mirrors review.recordExperienceOnLanding's guard shape: nil-safe on every
// dependency, degrades to a no-op (never blocks the caller's own status
// write) on any failure, and never captures work content for a work-typed
// project without scrubbing first.
func Capture(store *Store, cfg *config.Config, projects *project.Store, al *audit.Logger, logger *slog.Logger, cur task.Task, target, reason string, class OperatorActionClass) {
	defer func() {
		if r := recover(); r != nil && logger != nil {
			logger.Warn("intervention.record.panic", "task_id", cur.ID, "panic", r)
		}
	}()
	if store == nil || cfg == nil || !cfg.Intervention.Enabled {
		return
	}
	if cur.ProjectID == "" || projects == nil {
		return
	}
	proj, err := projects.Get(cur.ProjectID)
	if err != nil || proj.ID == "" {
		logCapture(al, logger, audit.EventInterventionSkipped, cur.ID, map[string]any{
			"reason": "project_unresolved",
		})
		return
	}
	if !cfg.AllowsProjectType(string(proj.Type)) {
		return
	}

	rec := FromUnblock(cur, proj, target, reason, class, time.Now().UTC())
	projectKey := ProjectKey(proj)
	if proj.Type == project.ProjectTypeWork {
		scrubRecord(&rec, proj.WorkBlocklist())
	}
	if err := store.Put(projectKey, rec); err != nil {
		logCapture(al, logger, audit.EventInterventionSkipped, cur.ID, map[string]any{
			"reason": "write_failed",
		})
		if logger != nil {
			logger.Warn("intervention.record.write", "task_id", cur.ID, "err", err)
		}
		return
	}
	// operator_action_class is the durable provenance signal downstream
	// consumers (e.g. the evaluation scorecard, issue #2727) key on to tell a
	// genuine operator unblock apart from an automated-recovery re-entry —
	// the fingerprint/project fields alone don't carry who resolved it.
	data := map[string]any{"fingerprint": rec.Fingerprint, "operator_action_class": string(class)}
	if proj.Type == project.ProjectTypeWork {
		data["project_key"] = projectKey
	} else {
		data["project_id"] = cur.ProjectID
	}
	logCapture(al, logger, audit.EventInterventionRecorded, cur.ID, data)
}

func logCapture(al *audit.Logger, logger *slog.Logger, eventType, taskID string, data map[string]any) {
	if al == nil {
		return
	}
	if err := al.Log(audit.Event{Type: eventType, TaskID: taskID, Data: data}); err != nil && logger != nil {
		logger.Warn("task.intervention.audit", "task_id", taskID, "err", err)
	}
}

// scrubRecord redacts every free-text field of rec against blocklist and
// replaces TaskID with an opaque, deterministic stand-in so a work-typed
// intervention record never carries the plain Sybra task ID (or any other
// work identifier) as a correlation handle back to the originating work
// task. Fingerprint is left untouched — it is built from short,
// code-authored tokens (blocker kind/code, status names), never work
// content, and must stay stable across scrubbed and unscrubbed records for
// dedup to keep working.
func scrubRecord(rec *Record, blocklist []string) {
	rec.TaskID = WorkRecordID(rec.TaskID)
	rec.ProjectID, _ = scrub.Scrub(rec.ProjectID, blocklist)
	rec.ProjectType, _ = scrub.Scrub(rec.ProjectType, blocklist)
	rec.BlockerKind, _ = scrub.Scrub(rec.BlockerKind, blocklist)
	rec.BlockerCode, _ = scrub.Scrub(rec.BlockerCode, blocklist)
	rec.FromStatus, _ = scrub.Scrub(rec.FromStatus, blocklist)
	rec.ToStatus, _ = scrub.Scrub(rec.ToStatus, blocklist)
	rec.WorkflowStep, _ = scrub.Scrub(rec.WorkflowStep, blocklist)
	rec.OperatorReason, _ = scrub.Scrub(rec.OperatorReason, blocklist)
	for i := range rec.AttemptedActions {
		rec.AttemptedActions[i], _ = scrub.Scrub(rec.AttemptedActions[i], blocklist)
	}
}
