package sybra

func (a *App) wirePlanningService() {
	a.planSvc.engine = a.workflowEngine
	a.planSvc.tasks = a.tasks
	a.planSvc.agents = a.agents
}
