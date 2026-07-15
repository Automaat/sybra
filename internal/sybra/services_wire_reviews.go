package sybra

func (a *App) wireReviewServices() {
	a.reviewer.WorkflowEngine = a.workflowEngine
	a.reviewSvc.reviewer = a.reviewer
	a.reviewSvc.tasks = a.tasks
}
