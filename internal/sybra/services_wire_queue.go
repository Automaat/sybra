package sybra

func (a *App) wireQueueService() {
	a.queueSvc.queue = a.agentQueue
}
