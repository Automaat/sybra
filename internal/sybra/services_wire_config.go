package sybra

func (a *App) wireConfigService() {
	a.configSvc.cfg = a.cfg
	a.configSvc.persisted = cloneConfig(a.cfg)
	a.configSvc.logLevel = a.logLevel
	a.configSvc.notifier = a.notifier
	a.configSvc.agents = a.agents
	a.configSvc.limits = a.limits
	a.configSvc.workflowEngine = a.workflowEngine
	a.configSvc.logger = a.logger
	a.configSvc.policy = a.limitPolicy
	// Read a.routingSvc lazily: it is constructed later in startRoutingService,
	// so binding the pointer now would capture nil. ApplyPersistedOverlay itself
	// no-ops when routing is disabled or no overlay was ever saved.
	a.configSvc.reapplyRouting = func() {
		if a.routingSvc != nil {
			a.routingSvc.ApplyPersistedOverlay()
		}
	}
}
