package sybra

func (a *App) wireLoopAgentService() {
	// The bound methods take no context — Wails calls them from the frontend —
	// so the service carries the app context to hand to a database-backed
	// repository, which would otherwise have no cancellation path at shutdown.
	a.loopAgentSvc.ctx = a.ctx
	a.loopAgentSvc.store = a.loopAgents
	a.loopAgentSvc.sched = a.loopSched
	a.loopAgentSvc.auditDir = a.auditDir
	a.loopAgentSvc.logger = a.logger
}
