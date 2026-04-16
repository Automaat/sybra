package poll

import (
	"context"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"
)

// classifierFactory lets tests inject a fake classifier. Production
// callers leave it nil and the handler constructs a *triage.ClaudeClassifier.
type classifierFactory func(model string, logger *slog.Logger) triage.Classifier

// TriageHandler runs the auto-triage loop. Each poll cycle:
//  1. Lists tasks in status=new.
//  2. For each, runs triage.Classify + triage.Apply.
//  3. Writes a triage.classified audit event.
//
// Classification is serial to avoid rate-limit surprises. The worker is
// opt-in per machine via config.Triage.Enabled.
type TriageHandler struct {
	tasks      *task.Manager
	projects   *project.Store
	cfg        *config.TriageConfig
	logger     *slog.Logger
	audit      *audit.Logger
	factory    classifierFactory
	perTaskTTL time.Duration
}

// NewTriageHandler constructs a TriageHandler.
func NewTriageHandler(
	tasks *task.Manager,
	projects *project.Store,
	al *audit.Logger,
	logger *slog.Logger,
	cfg *config.TriageConfig,
) *TriageHandler {
	return &TriageHandler{
		tasks:      tasks,
		projects:   projects,
		cfg:        cfg,
		logger:     logger,
		audit:      al,
		perTaskTTL: 2 * time.Minute,
	}
}

// Name implements poll.Fetcher.
func (h *TriageHandler) Name() string { return "triage" }

// Poll implements poll.Fetcher. Returns the next poll interval.
func (h *TriageHandler) Poll(ctx context.Context) time.Duration {
	interval := time.Duration(h.cfg.PollSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}

	tasks, err := h.tasks.List()
	if err != nil {
		h.logger.Warn("triage.list", "err", err)
		return interval
	}

	projects, err := h.projects.List()
	if err != nil {
		h.logger.Warn("triage.projects", "err", err)
		return interval
	}

	classifier := h.buildClassifier()

	var classified int
	for i := range tasks {
		if ctx.Err() != nil {
			return interval
		}
		t := tasks[i]
		if t.Status != task.StatusNew {
			continue
		}
		if !t.UpdatedAt.IsZero() && time.Since(t.UpdatedAt) < 5*time.Second {
			continue
		}
		h.classifyOne(ctx, classifier, t, projects)
		classified++
	}

	if classified > 0 {
		h.logger.Info("triage.poll", "classified", classified)
	}
	return interval
}

func (h *TriageHandler) buildClassifier() triage.Classifier {
	if h.factory != nil {
		return h.factory(h.cfg.Model, h.logger)
	}
	return &triage.ClaudeClassifier{Model: h.cfg.Model, Logger: h.logger}
}

func (h *TriageHandler) classifyOne(
	parent context.Context,
	classifier triage.Classifier,
	t task.Task,
	projects []project.Project,
) {
	ctx, cancel := context.WithTimeout(parent, h.perTaskTTL)
	defer cancel()

	v, err := classifier.Classify(ctx, t, projects)
	if err != nil {
		h.logger.Warn("triage.classify", "task", t.ID, "err", err)
		return
	}
	updated, err := triage.Apply(h.tasks, t, v, projects)
	if err != nil {
		h.logger.Warn("triage.apply", "task", t.ID, "err", err)
		return
	}
	if h.audit != nil {
		_ = h.audit.Log(audit.Event{
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
}
