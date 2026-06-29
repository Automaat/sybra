package sybra

func (a *App) wireWorkflowService() {
	a.workflowSvc.engine = a.workflowEngine
	a.workflowSvc.store = a.workflowStore
}
