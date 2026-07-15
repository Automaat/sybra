package sybra

func (a *App) wireLoopAgentService() {
	a.loopAgentSvc.store = a.loopAgents
	a.loopAgentSvc.sched = a.loopSched
	a.loopAgentSvc.auditDir = a.auditDir
	a.loopAgentSvc.logger = a.logger
}
