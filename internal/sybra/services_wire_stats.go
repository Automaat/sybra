package sybra

func (a *App) wireStatsService() {
	a.statsSvc.stats = a.stats
	a.statsSvc.limits = a.limits
	a.statsSvc.projects = a.projects
	a.statsSvc.tasks = a.tasks
	a.statsSvc.auditDir = a.auditDir
	a.statsSvc.policy = a.limitPolicy
	a.statsSvc.currentConfig = a.currentConfig
	a.auditSvc.auditDir = a.auditDir
	a.selfMonSvc.tasks = a.tasks
	a.selfMonSvc.logger = a.logger
	a.selfMonSvc.currentConfig = a.currentConfig
}
