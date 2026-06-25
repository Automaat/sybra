package sybra

import (
	"maps"

	"github.com/Automaat/sybra/internal/httpapi"
)

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
	a.browserSvc.open = a.openBrowser

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
// Each service carries an explicit method allowlist — only listed methods are
// reachable; all other exported methods return 404.
func ServiceRegistry(a *App) map[string]httpapi.Service {
	out := make(map[string]httpapi.Service, 13)
	maps.Copy(out, a.coreHTTPServices())
	maps.Copy(out, a.planningHTTPServices())
	maps.Copy(out, a.projectHTTPServices())
	return out
}

func (a *App) coreHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"App": httpapi.NewService(a,
			"GetMonitorReport",
			"GetEvaluationReport",
			"StartAgent",
			"StartChat",
			"StopChat",
			"ListBackgroundOps",
			"ListNotifications",
			"SetDesktopNotifications",
		),
		"AgentService": httpapi.NewService(a.agentSvc,
			"StopAgent",
			"ListAgents",
			"DiscoverAgents",
			"GetAgentOutput",
			"SendMessage",
			"RespondApproval",
			"GetConvoOutput",
			"GetAgentRunLog",
			"GetAgentRunConvoLog",
			"RespondEscalation",
			"GetAgentDiff",
			// OpenWorktree and ResumeInClaudeCode open local GUI apps.
		),
		"ConfigService": httpapi.NewService(a.configSvc,
			"GetSettings",
			"UpdateTodoistToken",
			"UpdateSettings",
		),
		"InfoService": httpapi.NewService(a.infoSvc,
			"GetVersion",
			"GetCodexModels",
			"GetCopilotModels",
		),
		"TaskService": httpapi.NewService(a.taskSvc,
			"ListTasks",
			"GetTask",
			"CreateTask",
			"UpdateTask",
			"DeleteTask",
		),
		"StatsService": httpapi.NewService(a.statsSvc,
			"GetStats",
		),
	}
}

func (a *App) planningHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"PlanningService": httpapi.NewService(a.planSvc,
			"TriageTask",
			"PlanTask",
			"ApprovePlan",
			"RejectPlan",
			"SendPlanMessage",
			"HasLivePlanAgent",
			"ApproveTestPlan",
			"RejectTestPlan",
			"SendTestPlanMessage",
			"HasLiveTestPlanAgent",
		),
		"ReviewService": httpapi.NewService(a.reviewSvc,
			"StartReview",
			"StartFixReview",
			"ListReviewComments",
			"AddReviewComment",
			"ResolveReviewComment",
			"DeleteReviewComment",
			"FetchReviews",
			"MarkPRReady",
		),
		"OrchestratorService": httpapi.NewService(a.orchSvc,
			"StartOrchestrator",
			"StopOrchestrator",
			"IsOrchestratorRunning",
			"GetOrchestratorAgentID",
		),
		"WorkflowService": httpapi.NewService(a.workflowSvc,
			"ListWorkflows",
			"GetWorkflow",
			"SaveWorkflow",
			"DeleteWorkflow",
			"ResetBuiltin",
			"StartWorkflow",
			"HandleHumanAction",
		),
	}
}

func (a *App) projectHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"ProjectService": httpapi.NewService(a.projectSvc,
			"ListProjects",
			"GetProject",
			"CreateProject",
			"UpdateProject",
			"SetProjectWorktreeBaseRef",
			"DeleteProject",
			"ListWorktrees",
			// SetProjectSetupCommands excluded: persists shell commands executed
			// via sh -c during worktree prep — Wails IPC only.
			// SetProjectSandboxConfig excluded: cfg.Deploy is run via sh -c in
			// k8s sandbox and Docker build/compose paths accept attacker-controlled
			// filesystem paths — Wails IPC only.
			// OpenInTerminal and OpenInEditor open local GUI apps.
		),
		"IntegrationService": httpapi.NewService(a.intgSvc,
			"SyncTodoist",
			"GetTodoistProjects",
			"TodoistEnabled",
			"FetchRenovatePRs",
			"MergeRenovatePR",
			"ApproveRenovatePR",
			"RerunRenovateChecks",
			"FixRenovateCI",
			"FetchAssignedIssues",
			"GetProviderHealth",
			"ProviderHealthEnabled",
			"SetProviderAutoFailover",
			"SetProviderEnabled",
		),
		"LoopAgentService": httpapi.NewService(a.loopAgentSvc,
			"ListLoopAgents",
			"GetLoopAgent",
			"CreateLoopAgent",
			"UpdateLoopAgent",
			"DeleteLoopAgent",
			"RunLoopAgentNow",
			"ListLoopAgentRuns",
		),
	}
}
