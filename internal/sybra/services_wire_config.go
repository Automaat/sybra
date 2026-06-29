package sybra

func (a *App) wireConfigService() {
	a.configSvc.cfg = a.cfg
	a.configSvc.logLevel = a.logLevel
	a.configSvc.notifier = a.notifier
	a.configSvc.agents = a.agents
	a.configSvc.limits = a.limits
	a.configSvc.logger = a.logger
	a.configSvc.policy = a.limitPolicy
	a.configSvc.reloadHook = a.reloadTodoist
}
