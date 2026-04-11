package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
)

const (
	renovatePollFast = 1 * time.Minute
	renovatePollSlow = 5 * time.Minute
)

// RenovateHandler manages Renovate PR polling and actions.
type RenovateHandler struct {
	projects *project.Store
	logger   *slog.Logger
	emit     func(string, any)
	cfg      *config.RenovateConfig
}

func newRenovateHandler(
	projects *project.Store,
	logger *slog.Logger,
	emit func(string, any),
	cfg *config.RenovateConfig,
) *RenovateHandler {
	return &RenovateHandler{
		projects: projects,
		logger:   logger,
		emit:     emit,
		cfg:      cfg,
	}
}

func (h *RenovateHandler) repos() []string {
	projects, err := h.projects.List()
	if err != nil {
		h.logger.Error("renovate.list-projects", "err", err)
		return nil
	}
	repos := make([]string, 0, len(projects))
	for i := range projects {
		repos = append(repos, projects[i].Owner+"/"+projects[i].Repo)
	}
	return repos
}

func (h *RenovateHandler) Name() string { return "renovate" }

func (h *RenovateHandler) Poll(_ context.Context) time.Duration {
	return h.pollRenovatePRs()
}

func (h *RenovateHandler) pollRenovatePRs() time.Duration {
	repos := h.repos()
	if len(repos) == 0 {
		return renovatePollSlow
	}

	prs, err := github.FetchRenovatePRs(h.cfg.Author, repos)
	if err != nil {
		h.logger.Warn("renovate.fetch", "err", err)
		return renovatePollSlow
	}

	h.emit(events.RenovateUpdated, prs)
	h.logger.Debug("renovate.poll", "count", len(prs))

	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return renovatePollFast
		}
	}
	return renovatePollSlow
}

func (a *App) initRenovate(emit func(string, any)) {
	if !a.cfg.Renovate.Enabled {
		return
	}
	a.renovateHandler = newRenovateHandler(a.projects, a.logger, emit, &a.cfg.Renovate)
	a.logger.Info("renovate.enabled", "author", a.cfg.Renovate.Author)
}
