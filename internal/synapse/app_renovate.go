package synapse

import "github.com/Automaat/synapse/internal/poll"

func (a *App) initRenovate(emit func(string, any)) {
	if !a.cfg.Renovate.Enabled {
		return
	}
	a.renovateHandler = poll.NewRenovateHandler(a.projects, a.logger, emit, &a.cfg.Renovate, a.allowsProjectType)
	a.logger.Info("renovate.enabled", "author", a.cfg.Renovate.Author)
}
