package sybra

func (a *App) wireIntegrationService() {
	a.intgSvc.tasks = a.tasks
	a.intgSvc.projects = a.projects
	a.intgSvc.agents = a.agents
	a.intgSvc.worktrees = a.worktrees
	a.intgSvc.audit = a.audit
	a.intgSvc.cfg = a.cfg
	a.intgSvc.logger = a.logger
	a.intgSvc.todoistHandler = a.todoistHandler
	a.intgSvc.renovateHandler = a.renovateHandler
	a.intgSvc.workflowEngine = a.workflowEngine
	a.intgSvc.providerHealth = a.providerHealth
	// Read a.cfg inside the closure so config reloads save the current config.
	a.intgSvc.saveConfig = func() error { return a.cfg.Save() }
}
