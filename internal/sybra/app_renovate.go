package sybra

import (
	"log/slog"
	"sync"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
)

// renovateCoordinator owns the Renovate poll handler. Its handler lifetime is
// managed by the shared poll hub, so it has no stop/reload path.
type renovateCoordinator struct {
	mu                    sync.Mutex
	projects              *project.Store
	logger                *slog.Logger
	emit                  func(string, any)
	cfg                   *config.Config
	allowsProjectType     func(project.ProjectType) bool
	handler               *poll.RenovateHandler
	monitorTransientFails int
}

func newRenovateCoordinator(
	projects *project.Store,
	logger *slog.Logger,
	emit func(string, any),
	cfg *config.Config,
	allowsProjectType func(project.ProjectType) bool,
) *renovateCoordinator {
	return &renovateCoordinator{
		projects:          projects,
		logger:            logger,
		emit:              emit,
		cfg:               cfg,
		allowsProjectType: allowsProjectType,
	}
}

func (c *renovateCoordinator) init() {
	if !c.cfg.Renovate.Enabled {
		return
	}
	c.monitorTransientFails = 0
	c.handler = poll.NewRenovateHandler(c.projects, c.logger, c.emit, &c.cfg.Renovate, c.allowsProjectType)
	c.handler.SetIntervals(c.cfg.GitHub.RenovateFast(), c.cfg.GitHub.RenovateSlow())
	c.logger.Info("renovate.enabled", "author", c.cfg.Renovate.Author)
}

func (c *renovateCoordinator) poller() *poll.RenovateHandler {
	if c == nil {
		return nil
	}
	return c.handler
}

func (c *renovateCoordinator) repos() []string {
	if c == nil || c.handler == nil {
		return nil
	}
	return c.handler.Repos()
}

func (c *renovateCoordinator) lastFetchedCount() func() int64 {
	if c == nil || c.handler == nil {
		return nil
	}
	return c.handler.LastFetchedCount
}

func (c *renovateCoordinator) prsForMonitor() []github.PullRequest {
	repos := c.repos()
	if len(repos) == 0 {
		return nil
	}
	rps, err := github.FetchRenovatePRs(c.cfg.Renovate.Author, repos)
	if err != nil {
		c.recordMonitorFetchError(err)
		return nil
	}
	c.resetMonitorFetchErrors()
	prs := make([]github.PullRequest, len(rps))
	for i := range rps {
		prs[i] = rps[i].PullRequest
	}
	return prs
}

func (c *renovateCoordinator) recordMonitorFetchError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if github.IsTransientError(err) {
		c.monitorTransientFails++
		if c.monitorTransientFails < transientFetchWarnThreshold {
			c.logger.Info("pr-monitor.renovate-fetch", "err", err)
		} else {
			c.logger.Warn("pr-monitor.renovate-fetch", "err", err, "consecutive", c.monitorTransientFails)
		}
		return
	}
	c.monitorTransientFails = 0
	c.logger.Warn("pr-monitor.renovate-fetch", "err", err)
}

func (c *renovateCoordinator) resetMonitorFetchErrors() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.monitorTransientFails = 0
}

func (a *App) initRenovate(emit func(string, any)) {
	a.renovate = newRenovateCoordinator(a.projects, a.logger, emit, a.cfg, a.allowsProjectType)
	a.renovate.init()
}

// renovatePRsForMonitor returns Renovate-bot PRs flattened to PullRequest for
// the pr-monitor's match pass. FetchReviews uses author:@me which excludes
// bots, so without this hook a Renovate PR linked to a task by pr_number
// never gets re-dispatched to pr-fix when CI keeps failing. Returns nil when
// renovate is disabled or no projects are registered for this machine.
func (a *App) renovatePRsForMonitor() []github.PullRequest {
	return a.renovate.prsForMonitor()
}
