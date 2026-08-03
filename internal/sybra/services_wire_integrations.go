package sybra

func (a *App) wireIntegrationService() {
	a.intgSvc.tasks = a.tasks
	a.intgSvc.projects = a.projects
	a.intgSvc.agents = a.agents
	a.intgSvc.worktrees = a.worktrees
	a.intgSvc.audit = a.audit
	a.intgSvc.cfg = a.currentConfig()
	a.intgSvc.currentConfig = a.currentConfig
	a.intgSvc.logger = a.logger
	a.intgSvc.renovate = a.renovate
	a.intgSvc.workflowEngine = a.workflowEngine
	a.intgSvc.providerHealth = a.providerHealth
	// Read the current snapshot inside the closure so config reloads save the
	// current config without mutating a shared config object.
	a.intgSvc.saveConfig = func() error { return a.currentConfig().Save() }
}
