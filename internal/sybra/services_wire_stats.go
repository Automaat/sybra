package sybra

func (a *App) wireStatsService() {
	a.statsSvc.stats = a.stats
	a.statsSvc.limits = a.limits
	a.statsSvc.projects = a.projects
	a.statsSvc.tasks = a.tasks
	a.statsSvc.policy = a.limitPolicy
}
