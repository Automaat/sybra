package sybra

func (a *App) wireAgentService() {
	a.agentSvc.agents = a.agents
	a.agentSvc.logger = a.logger
	a.agentSvc.tasks = a.tasks
	a.agentSvc.cfg = a.cfg
	a.agentSvc.logsDir = a.logDir
	a.agentSvc.worktrees = a.worktrees
}

func (a *App) wireOrchestratorService(emit func(string, any)) {
	a.orchSvc.agents = a.agents
	a.orchSvc.audit = a.audit
	a.orchSvc.logger = a.logger
	a.orchSvc.emit = emit
}

func (a *App) wireAgentOrchestrator() {
	a.agentOrch.Sandboxes = a.sandboxes
	a.agentOrch.Bgops = a.bgops
}
