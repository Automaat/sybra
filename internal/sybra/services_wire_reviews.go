package sybra

func (a *App) wireReviewServices() {
	a.reviewer.workflowEngine = a.workflowEngine
	a.reviewSvc.reviewer = a.reviewer
	a.reviewSvc.tasks = a.tasks
}
