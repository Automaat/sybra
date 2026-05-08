package sybra

// wireServices populates the Wails-bound service structs that were pre-allocated
// in NewApp(). Must be called after all dependencies are initialized.
func (a *App) wireServices(emit func(string, any)) {
	a.reviewer.workflowEngine = a.workflowEngine
	a.reviewSvc.reviewer = a.reviewer
	a.reviewSvc.tasks = a.tasks
	a.taskSvc.tasks = a.tasks
	a.taskSvc.agents = a.agents
	a.taskSvc.workflowEngine = a.workflowEngine
	a.taskSvc.worktrees = a.worktrees
	a.taskSvc.sandboxes = a.sandboxes
	a.taskSvc.wg = &a.wg
	a.taskSvc.logger = a.logger
	a.taskSvc.audit = a.audit
	a.planSvc.engine = a.workflowEngine
	a.planSvc.tasks = a.tasks
	a.planSvc.agents = a.agents
	a.agentSvc.agents = a.agents
	a.agentSvc.logger = a.logger
	a.agentSvc.tasks = a.tasks
	a.agentSvc.cfg = a.cfg
	a.agentSvc.logsDir = a.logDir
	a.agentSvc.worktrees = a.worktrees
	a.orchSvc.agents = a.agents
	a.orchSvc.audit = a.audit
	a.orchSvc.logger = a.logger
	a.orchSvc.emit = emit
	a.agentOrch.sandboxes = a.sandboxes
	a.projectSvc.projects = a.projects
	a.projectSvc.worktrees = a.worktrees
	a.projectSvc.logger = a.logger
	a.projectSvc.notifier = a.notifier
	a.projectSvc.bgops = a.bgops
	a.projectSvc.wg = &a.wg
	a.agentOrch.bgops = a.bgops
	a.loopAgentSvc.store = a.loopAgents
	a.loopAgentSvc.sched = a.loopSched
	a.loopAgentSvc.auditDir = a.auditDir
	a.loopAgentSvc.logger = a.logger
	a.configSvc.cfg = a.cfg
	a.configSvc.logLevel = a.logLevel
	a.configSvc.notifier = a.notifier
	a.configSvc.agents = a.agents
	a.configSvc.logger = a.logger
	a.configSvc.reloadHook = a.reloadTodoist
	a.intgSvc.tasks = a.tasks
	a.intgSvc.projects = a.projects
	a.intgSvc.agents = a.agents
	a.intgSvc.worktrees = a.worktrees
	a.intgSvc.audit = a.audit
	a.intgSvc.cfg = a.cfg
	a.intgSvc.logger = a.logger
	a.intgSvc.todoistHandler = a.todoistHandler
	a.intgSvc.renovateHandler = a.renovateHandler
	a.intgSvc.workflowEngine = a.workflowEngine
	a.intgSvc.providerHealth = a.providerHealth
	a.intgSvc.saveConfig = func() error { return a.cfg.Save() }
	a.statsSvc.stats = a.stats
	a.statsSvc.projects = a.projects
	a.workflowSvc.engine = a.workflowEngine
	a.workflowSvc.store = a.workflowStore

	// Subscribe handlers to manager callbacks last — by this point every
	// dependency the handler reads is wired up, so the closures can't
	// observe a partially-constructed App.
	a.agentCompletion = a.newAgentCompletionHandler(emit)
	a.agents.SetOnComplete(a.agentCompletion.OnComplete)
	if a.workflowEngine != nil {
		a.workflowEngine.SetOnComplete(a.agentCompletion.OnWorkflowComplete)
	}
}

// ServiceRegistry returns the named service instances for HTTP dispatch.
// All values are the concrete pointers Wails binds; the HTTP handler uses
// reflection to call their exported methods.
func (a *App) ServiceRegistry() map[string]any {
	return map[string]any{
		"App":                 a,
		"AgentService":        a.agentSvc,
		"ConfigService":       a.configSvc,
		"InfoService":         a.infoSvc,
		"IntegrationService":  a.intgSvc,
		"LoopAgentService":    a.loopAgentSvc,
		"OrchestratorService": a.orchSvc,
		"PlanningService":     a.planSvc,
		"ProjectService":      a.projectSvc,
		"ReviewService":       a.reviewSvc,
		"StatsService":        a.statsSvc,
		"TaskService":         a.taskSvc,
		"WorkflowService":     a.workflowSvc,
	}
}
