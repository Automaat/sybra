package sybra

func (a *App) wireAgentServices(emit func(string, any)) {
	a.agentSvc.agents = a.agents
	a.agentSvc.logger = a.logger
	a.agentSvc.tasks = a.tasks
	a.agentSvc.cfg = a.cfg
	a.agentSvc.logsDir = a.logDir
	a.agentSvc.worktrees = a.worktrees

	a.orchSvc.agents = a.agents
	a.orchSvc.audit = a.audit
	a.orchSvc.logger = a.logger
	a.orchSvc.emit = emit

	a.agentOrch.sandboxes = a.sandboxes
	a.agentOrch.bgops = a.bgops
}
