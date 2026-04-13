package poll

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/metrics"
	"github.com/Automaat/synapse/internal/project"
)

const (
	RenovatePollFast = 1 * time.Minute
	RenovatePollSlow = 5 * time.Minute
)

// RenovateHandler manages Renovate PR polling and actions.
type RenovateHandler struct {
	projects     *project.Store
	logger       *slog.Logger
	emit         func(string, any)
	cfg          *config.RenovateConfig
	allowsType   func(project.ProjectType) bool
	lastPRsCount atomic.Int64
}

// NewRenovateHandler creates a RenovateHandler. allowsType filters which
// project types this machine handles; nil means all types.
func NewRenovateHandler(
	projects *project.Store,
	logger *slog.Logger,
	emit func(string, any),
	cfg *config.RenovateConfig,
	allowsType func(project.ProjectType) bool,
) *RenovateHandler {
	if allowsType == nil {
		allowsType = func(project.ProjectType) bool { return true }
	}
	return &RenovateHandler{
		projects:   projects,
		logger:     logger,
		emit:       emit,
		cfg:        cfg,
		allowsType: allowsType,
	}
}

// Repos returns owner/repo strings for registered projects whose type is
// allowed on this machine.
func (h *RenovateHandler) Repos() []string {
	projects, err := h.projects.List()
	if err != nil {
		h.logger.Error("renovate.list-projects", "err", err)
		return nil
	}
	repos := make([]string, 0, len(projects))
	for i := range projects {
		if !h.allowsType(projects[i].Type) {
			continue
		}
		repos = append(repos, projects[i].Owner+"/"+projects[i].Repo)
	}
	return repos
}

func (h *RenovateHandler) Name() string { return "renovate" }

func (h *RenovateHandler) Poll(_ context.Context) time.Duration {
	return h.pollRenovatePRs()
}

// LastFetchedCount returns the most recent Renovate PR count observed by a
// successful poll. Zero until the first successful poll.
func (h *RenovateHandler) LastFetchedCount() int64 {
	return h.lastPRsCount.Load()
}

func (h *RenovateHandler) pollRenovatePRs() time.Duration {
	repos := h.Repos()
	if len(repos) == 0 {
		return RenovatePollSlow
	}

	prs, err := github.FetchRenovatePRs(h.cfg.Author, repos)
	metrics.RenovatePoll(err == nil)
	if err != nil {
		h.logger.Warn("renovate.fetch", "err", err)
		return RenovatePollSlow
	}

	h.lastPRsCount.Store(int64(len(prs)))
	h.emit(events.RenovateUpdated, prs)
	h.logger.Debug("renovate.poll", "count", len(prs))

	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return RenovatePollFast
		}
	}
	return RenovatePollSlow
}
