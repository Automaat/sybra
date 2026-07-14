package sybra

func (a *App) wireAgentService() {
	a.agentSvc.agents = a.agents
	a.agentSvc.logger = a.logger
	a.agentSvc.tasks = a.tasks
	a.agentSvc.cfg = a.cfg
	a.agentSvc.logsDir = a.logDir
	a.agentSvc.worktrees = a.worktrees
	a.agentSvc.artifacts = a.artifacts
}

func (a *App) wireOrchestratorService(emit func(string, any)) {
	a.orchSvc.agents = a.agents
	a.orchSvc.audit = a.audit
	a.orchSvc.logger = a.logger
	a.orchSvc.emit = emit
	if a.cfg != nil {
		a.orchSvc.abTesting = a.cfg.ABTesting
	}
}

func (a *App) wireAgentOrchestrator() {
	a.agentOrch.SetSandboxes(a.sandboxes)
	a.agentOrch.SetBgops(a.bgops)
}
