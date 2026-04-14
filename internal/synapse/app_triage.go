package synapse

import "github.com/Automaat/synapse/internal/poll"

// initTriage constructs the background auto-triage handler if enabled.
// The handler is registered with the poll hub in startPollHub alongside
// renovate and the issues fetcher.
func (a *App) initTriage() {
	if !a.cfg.Triage.Enabled {
		return
	}
	a.triageHandler = poll.NewTriageHandler(a.tasks, a.projects, a.audit, a.logger, &a.cfg.Triage)
	a.logger.Info("triage.enabled", "poll_seconds", a.cfg.Triage.PollSeconds, "model", a.cfg.Triage.Model)
}
