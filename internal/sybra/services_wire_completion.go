package sybra

func (a *App) wireCompletionHandlers(emit func(string, any)) {
	// Build completion handlers last: by this point every dependency they read
	// is wired up, and the manager's construction-time callback delegates to
	// a.agentCompletion once it is populated here.
	a.agentCompletion = a.newAgentCompletionHandler(emit)
	if a.workflowEngine != nil {
		a.workflowEngine.SetOnComplete(a.agentCompletion.OnWorkflowComplete)
	}
}
