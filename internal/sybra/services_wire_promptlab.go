package sybra

func (a *App) wirePromptLabService() {
	a.promptLabSvc.tasks = a.tasks
	a.promptLabSvc.artifacts = a.artifacts
	a.promptLabSvc.projects = a.projects
	a.promptLabSvc.workflowEngine = a.workflowEngine
	a.promptLabSvc.stats = a.stats
	a.promptLabSvc.currentConfig = a.currentConfig
}
