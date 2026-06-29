package sybra

import (
	"maps"

	"github.com/Automaat/sybra/internal/httpapi"
)

// wireServices populates the Wails-bound service structs that were pre-allocated
// in NewApp(). Must be called after all dependencies are initialized.
func (a *App) wireServices(emit func(string, any)) {
	a.wireReviewServices()
	a.wireTaskService()
	a.wirePlanningService()
	a.wireAgentAndOrchestratorServices(emit)
	a.wireProjectServices()
	a.wireLoopAgentService()
	a.wireConfigService()
	a.wireIntegrationService()
	a.wireStatsService()
	a.wireWorkflowService()
	a.wireBrowserService()
	// MUST be last: completion handlers read fully-wired service dependencies.
	a.wireCompletionHandlers(emit)
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
			"GetLifecyclePhases",
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
