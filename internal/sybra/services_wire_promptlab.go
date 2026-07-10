package sybra

func (a *App) wirePromptLabService() {
	a.promptLabSvc.tasks = a.tasks
	a.promptLabSvc.artifacts = a.artifacts
}
