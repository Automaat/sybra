package poll

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/project"
)

const (
	RenovatePollFast = 1 * time.Minute
	RenovatePollSlow = 5 * time.Minute
)

const renovateTransientWarnThreshold = 3

// RenovateHandler manages Renovate PR polling and actions.
type RenovateHandler struct {
	projects            *project.Store
	logger              *slog.Logger
	emit                func(string, any)
	cfg                 *config.RenovateConfig
	allowsType          func(project.ProjectType) bool
	lastPRsCount        atomic.Int64
	transientFetchFails int
	authCircuit         *AuthCircuit
	// fast/slow are the resolved poll intervals (override-or-default). Zero
	// falls back to the package constants.
	fast time.Duration
	slow time.Duration
}

func (h *RenovateHandler) fastInterval() time.Duration {
	base := h.fast
	if base <= 0 {
		base = RenovatePollFast
	}
	return github.ScaleInterval(base)
}

func (h *RenovateHandler) slowInterval() time.Duration {
	base := h.slow
	if base <= 0 {
		base = RenovatePollSlow
	}
	return github.ScaleInterval(base)
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
		projects:    projects,
		logger:      logger,
		emit:        emit,
		cfg:         cfg,
		allowsType:  allowsType,
		authCircuit: NewAuthCircuit("renovate", logger),
	}
}

// SetIntervals overrides the fast/slow poll cadence. Zero values keep the
// package defaults. Called at wiring time from the resolved github config.
func (h *RenovateHandler) SetIntervals(fast, slow time.Duration) {
	h.fast, h.slow = fast, slow
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

// AuthCircuitOpen reports whether repeated GitHub auth failures have
// tripped this poller's circuit breaker (see poll.AuthCircuit).
func (h *RenovateHandler) AuthCircuitOpen() bool { return h.authCircuit.Open() }

func (h *RenovateHandler) Poll(ctx context.Context) time.Duration {
	return h.pollRenovatePRs(ctx)
}

// LastFetchedCount returns the most recent Renovate PR count observed by a
// successful poll. Zero until the first successful poll.
func (h *RenovateHandler) LastFetchedCount() int64 {
	return h.lastPRsCount.Load()
}

func (h *RenovateHandler) pollRenovatePRs(ctx context.Context) time.Duration {
	repos := h.Repos()
	if len(repos) == 0 {
		return h.slowInterval()
	}

	prs, err := github.FetchRenovatePRs(ctx, h.cfg.Author, repos)
	metrics.RenovatePoll(ctx, err == nil)
	if err != nil {
		switch {
		case github.IsAuthError(err):
			h.authCircuit.RecordFailure(err)
			if h.authCircuit.Open() {
				return AuthCircuitBackoff
			}
			// Pre-trip: Info, not Warn, so up-to-threshold auth failures don't
			// flood before the circuit's single trip line.
			h.logger.Info("renovate.fetch", "err", err)
		case github.IsTransientError(err):
			h.transientFetchFails++
			if h.transientFetchFails < renovateTransientWarnThreshold {
				h.logger.Info("renovate.fetch", "err", err)
			} else {
				h.logger.Warn("renovate.fetch", "err", err, "consecutive", h.transientFetchFails)
			}
		default:
			h.transientFetchFails = 0
			h.logger.Warn("renovate.fetch", "err", err)
		}
		return h.slowInterval()
	}
	h.transientFetchFails = 0
	h.authCircuit.RecordSuccess()

	h.lastPRsCount.Store(int64(len(prs)))
	h.emit(events.RenovateUpdated, prs)
	h.logger.Debug("renovate.poll", "count", len(prs))

	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return h.fastInterval()
		}
	}
	return h.slowInterval()
}
