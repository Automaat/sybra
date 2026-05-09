package sybra

import (
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/poll"
)

func (a *App) initRenovate(emit func(string, any)) {
	if !a.cfg.Renovate.Enabled {
		return
	}
	a.renovateHandler = poll.NewRenovateHandler(a.projects, a.logger, emit, &a.cfg.Renovate, a.allowsProjectType)
	a.logger.Info("renovate.enabled", "author", a.cfg.Renovate.Author)
}

// renovatePRsForMonitor returns Renovate-bot PRs flattened to PullRequest for
// the pr-monitor's match pass. FetchReviews uses author:@me which excludes
// bots, so without this hook a Renovate PR linked to a task by pr_number
// never gets re-dispatched to pr-fix when CI keeps failing. Returns nil when
// renovate is disabled or no projects are registered for this machine.
func (a *App) renovatePRsForMonitor() []github.PullRequest {
	if a.renovateHandler == nil {
		return nil
	}
	repos := a.renovateHandler.Repos()
	if len(repos) == 0 {
		return nil
	}
	rps, err := github.FetchRenovatePRs(a.cfg.Renovate.Author, repos)
	if err != nil {
		a.logger.Warn("pr-monitor.renovate-fetch", "err", err)
		return nil
	}
	prs := make([]github.PullRequest, len(rps))
	for i := range rps {
		prs[i] = rps[i].PullRequest
	}
	return prs
}
