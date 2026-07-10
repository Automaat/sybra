package triage

import (
	"context"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// ClassifyAndApply runs the classifier against t, applies the resulting verdict
// (title, tags, size/type, mode, project, routed status) in one atomic update,
// and writes the triage.classified audit event.
//
// It is the single shared happy-path for every triage entry point — the
// `sybra-cli triage classify` CLI, the poll-based auto-triage handler
// (internal/poll.TriageHandler), and the classify_task workflow step
// (internal/sybra.taskClassifierAdapter). Each caller layers its own divergent
// failure handling on top of the returned error (retryable status reason,
// log-and-skip, or route-to-human-required respectively), so only the
// happy-path — which had silently diverged as three near-identical copies —
// lives here. al may be nil.
func ClassifyAndApply(
	ctx context.Context,
	classifier Classifier,
	mgr *task.Manager,
	al *audit.Logger,
	t task.Task,
	projects []project.Project,
) (Verdict, task.Task, error) {
	v, err := classifier.Classify(ctx, t, projects)
	if err != nil {
		return Verdict{}, task.Task{}, err
	}
	updated, err := Apply(mgr, t, v, projects)
	if err != nil {
		return Verdict{}, task.Task{}, err
	}
	if al != nil {
		_ = al.Log(audit.Event{
			Type:   audit.EventTriageClassified,
			TaskID: t.ID,
			Data: map[string]any{
				"title":      v.Title,
				"tags":       v.Tags,
				"size":       v.Size,
				"type":       v.Type,
				"mode":       v.Mode,
				"project_id": updated.ProjectID,
				"status":     string(updated.Status),
			},
		})
	}
	return v, updated, nil
}
