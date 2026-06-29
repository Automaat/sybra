package sybra

func (a *App) wireProjectServices() {
	a.projectSvc.projects = a.projects
	a.projectSvc.worktrees = a.worktrees
	a.projectSvc.logger = a.logger
	a.projectSvc.notifier = a.notifier
	a.projectSvc.bgops = a.bgops
	a.projectSvc.wg = &a.wg
}
