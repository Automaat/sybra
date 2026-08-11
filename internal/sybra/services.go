package sybra

import (
	"maps"
	"net/http"

	"github.com/Automaat/sybra/internal/httpapi"
)

// wireServices populates the Wails-bound service structs that were pre-allocated
// in NewApp(). Must be called after all dependencies are initialized.
func (a *App) wireServices(emit func(string, any)) {
	go a.infoSvc.primeRuntimeSnapshot()
	a.wireReviewServices()
	a.wireTaskService()
	a.wirePlanningService()
	a.wireAgentService()
	a.wireOrchestratorService(emit)
	a.wireAgentOrchestrator()
	a.wireProjectServices()
	a.wireLoopAgentService()
	a.wireConfigService()
	a.wireIntegrationService()
	a.wireStatsService()
	a.wireWorkflowService()
	a.wireBrowserService()
	a.wireLearningService(emit)
	a.wirePromptLabService()
	a.wireQueueService()
	a.wireVerifierControl()
	// MUST be last: completion handlers read fully-wired service dependencies.
	a.wireCompletionHandlers(emit)
}

func (a *App) wireVerifierControl() {
	if a.agentSvc == nil || a.agentSvc.approval == nil {
		return
	}
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TaskService": httpapi.NewService(a.taskSvc, "GetTask", "UpdateTask").WithReadOnly("GetTask"),
	}, a.logger, nil)
	a.agentSvc.approval.SetVerifierControl(mux)
}

// ServiceRegistry returns the named service instances for HTTP dispatch.
// Each service carries an explicit method allowlist — only listed methods are
// reachable; all other exported methods return 404.
func ServiceRegistry(a *App) map[string]httpapi.Service {
	out := make(map[string]httpapi.Service)
	maps.Copy(out, a.coreHTTPServices())
	maps.Copy(out, a.planningHTTPServices())
	maps.Copy(out, a.projectHTTPServices())
	return out
}

func (a *App) coreHTTPServices() map[string]httpapi.Service {
	out := make(map[string]httpapi.Service)
	maps.Copy(out, a.coreAppHTTPServices())
	maps.Copy(out, a.coreTaskHTTPServices())
	maps.Copy(out, a.coreInfraHTTPServices())
	return out
}

func (a *App) coreAppHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"App": httpapi.NewService(a,
			"GetMonitorReport",
			"GetEvaluationReport",
			"GetLifecyclePhases",
			"GetAutonomyTrend",
			"GetLearningDigestStatus",
			// Both act on the host serving this board: one shells out to the
			// claude CLI, the other grabs an OS-wide key. Loopback-only below.
			"RunLearningDigestNow",
			"RegisterSpotlightHotkey",
			"StartAgent",
			"StartK8sPocAgent",
			"AgentQueueSnapshot",
			"ListBackgroundOps",
			"ListNotifications",
			"SetDesktopNotifications",
		).WithReadOnly(
			"GetMonitorReport",
			"GetEvaluationReport",
			"GetLifecyclePhases",
			"GetAutonomyTrend",
			"GetLearningDigestStatus",
			"AgentQueueSnapshot",
			"ListBackgroundOps",
			"ListNotifications",
		).WithLocalOnly(
			"RunLearningDigestNow",
			"RegisterSpotlightHotkey",
		),
		// OpenWorktree opens a GUI app on the host serving this board.
		"AgentService": httpapi.NewService(a.agentSvc,
			"StopAgent",
			"ListAgents",
			"DiscoverAgents",
			"GetAgent",
			"GetAgentOutput",
			"SendMessage",
			"RespondApproval",
			"GetConvoOutput",
			"GetAgentRunLog",
			"GetAgentRunConvoLog",
			"RespondEscalation",
			"GetAgentDiff",
			"OpenWorktree",
		).WithReadOnly(
			"ListAgents",
			"GetAgent",
			"GetAgentOutput",
			"GetConvoOutput",
			"GetAgentRunLog",
			"GetAgentRunConvoLog",
			"GetAgentDiff",
		).WithLocalOnly("OpenWorktree"),
		"BrowserService": httpapi.NewService(a.browserSvc, "Open", "OpenExternal").
			WithLocalOnly("Open", "OpenExternal"),
		"ConfigService": httpapi.NewService(a.configSvc,
			"GetSettings",
			"GetPathExplanations",
			"GetDefaultSettings",
			"UpdateSettings",
			"GetRawConfig",
			"SaveRawConfig",
		).WithReadOnly(
			"GetSettings",
			"GetPathExplanations",
			"GetDefaultSettings",
			"GetRawConfig",
		),
		"InfoService": httpapi.NewService(a.infoSvc,
			"GetVersion",
			"GetCodexModels",
			"GetCopilotModels",
			"GetAvailableRuntimes",
		).WithReadOnly(
			"GetVersion",
			"GetCodexModels",
			"GetCopilotModels",
			"GetAvailableRuntimes",
		),
	}
}

func (a *App) coreTaskHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"TaskService": httpapi.NewService(a.taskSvc,
			"AssignTask",
			"BlessTampering",
			"ListTasks",
			"ListTasksForNode",
			"ListTaskArtifacts",
			"GetTaskSetupLog",
			"ListTaskAuditEvents",
			"GetTamperReport",
			"GetTask",
			"RecoverLostAgent",
			"CreateTask",
			"UpdateTask",
			"DeleteTask",
			"UploadAttachment",
			"ListAttachments",
			"DeleteAttachment",
			"GetAttachmentURL",
			"DispatchFromHumanRequired",
			"ListTaskProgress",
			// sybra-cli board surface; see svc_tasks_board.go.
			"CreateTaskFull",
			"UpdateTaskFields",
			"ApplyTransition",
			"TouchTask",
			"ListTrash",
			"RestoreFromTrash",
			"DeleteTrashedGeneration",
			"PruneAllTrash",
			"ExpandUmbrella",
			"ClassifyTask",
			"ScanMonitor",
			"AppendTaskProgress",
			"ListTaskArtifactMetas",
			"ReadTaskArtifact",
			"ReindexTaskArtifacts",
			"ListTaskSnapshotHistory",
			"MapDuplicateIncidents",
		).WithReadOnly(
			"ListTasks",
			"ListTasksForNode",
			"ListTaskArtifacts",
			"GetTaskSetupLog",
			"ListTaskAuditEvents",
			"GetTamperReport",
			"GetTask",
			"ListAttachments",
			"GetAttachmentURL",
			"ListTaskProgress",
			"ListTrash",
			"ScanMonitor",
			"ListTaskArtifactMetas",
			"ReadTaskArtifact",
			"ListTaskSnapshotHistory",
		),
		"ClusterAttachmentService": httpapi.NewService(&ClusterAttachmentService{tasks: a.tasks, attachments: a.attachments, logger: a.logger},
			"ExportAttachment",
			"ImportAttachment",
		).WithReadOnly("ExportAttachment"),
		"StatsService": httpapi.NewService(a.statsSvc,
			"GetStats",
			"ScanEvaluation",
		).WithReadOnly("GetStats", "ScanEvaluation"),
		"AuditService": httpapi.NewService(a.auditSvc,
			"QueryAuditEvents",
		).WithReadOnly("QueryAuditEvents"),
		"SelfMonitorService": httpapi.NewService(a.selfMonSvc,
			"GetSelfMonitorReport",
			"InvestigateSelfMonitor",
			"ListSelfMonitorLedger",
			"RunHarnessEvolution",
		).WithReadOnly(
			"GetSelfMonitorReport",
			// InvestigateSelfMonitor persists nothing and files nothing —
			// it builds a judge-less, actor-less service for the preview.
			"InvestigateSelfMonitor",
			"ListSelfMonitorLedger",
		),
		"LearningService": httpapi.NewService(a.learningSvc,
			"ListDigests",
			"GetLatestDigest",
			// StoreDigest excluded: the raw store is never scrubbed at write
			// time (see internal/learning package doc) — Wails/local-only.
		).WithReadOnly("ListDigests", "GetLatestDigest"),
	}
}

func (a *App) coreInfraHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"QueueService": httpapi.NewService(a.queueSvc,
			"SnapshotDepth",
		).WithReadOnly("SnapshotDepth"),
		"ClusterService": httpapi.NewService(a.clusterSvc,
			"GetNodes",
			"ListNodeAgents",
			"GetAgentOnNode",
			"ReassignTask",
			"GetAgentOutputOnNode",
			"GetConvoOutputOnNode",
			"StopAgentOnNode",
			"SendMessageToNode",
			"RespondApprovalOnNode",
			"ApprovePlanOnNode",
			"RejectPlanOnNode",
		).WithReadOnly(
			"GetNodes",
			"ListNodeAgents",
			"GetAgentOnNode",
			"GetAgentOutputOnNode",
			"GetConvoOutputOnNode",
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
		).WithReadOnly("HasLivePlanAgent"),
		"ReviewService": httpapi.NewService(a.reviewSvc,
			"StartReview",
			"StartFixReview",
			"ListReviewComments",
			"AddReviewComment",
			"ResolveReviewComment",
			"DeleteReviewComment",
			"FetchReviews",
			"MarkPRReady",
		).WithReadOnly("ListReviewComments", "FetchReviews"),
		"OrchestratorService": httpapi.NewService(a.orchSvc,
			"StartOrchestrator",
			"StopOrchestrator",
			"IsOrchestratorRunning",
			"GetOrchestratorAgentID",
		).WithReadOnly("IsOrchestratorRunning", "GetOrchestratorAgentID"),
		"WorkflowService": httpapi.NewService(a.workflowSvc,
			"ListWorkflows",
			"GetWorkflow",
			"SaveWorkflow",
			"DeleteWorkflow",
			"ResetBuiltin",
			"StartWorkflow",
			"HandleHumanAction",
		).WithReadOnly("ListWorkflows", "GetWorkflow"),
		"PromptLabService": httpapi.NewService(a.promptLabSvc,
			"RunPromptLab",
			"ApproveProposal",
			"RejectProposal",
		),
	}
}

func (a *App) projectHTTPServices() map[string]httpapi.Service {
	return map[string]httpapi.Service{
		"ProjectService": httpapi.NewService(a.projectSvc,
			"ListProjects",
			"GetProject",
			"CreateProject",
			// CreateProjectAndClone waits for the clone. A CLI caller exits
			// as soon as the call returns, so the async variant would report
			// success on a repo that never cloned.
			"CreateProjectAndClone",
			"AdoptProject",
			"GetProjectRawType",
			"UpdateProject",
			"SetProjectWorktreeBaseRef",
			"DeleteProject",
			"ListWorktrees",
			// SetProjectSetupCommands is reachable here so `sybra-cli project
			// update --setup` works against a server rather than editing a
			// project file the owning instance never reads. The commands it
			// persists do run via sh -c during worktree prep, but a caller
			// holding the server token can already dispatch an agent that runs
			// anything, so withholding it bought no containment.
			"SetProjectSetupCommands",
			// SetProjectSandboxConfig runs cfg.Deploy via sh -c during k8s and
			// Docker sandbox setup, but withholding it only broke the Sandbox
			// tab: a caller holding the server token can already dispatch an
			// agent that runs anything, which is the same reasoning that put
			// SetProjectSetupCommands here.
			"SetProjectSandboxConfig",
			"OpenInTerminal",
			"OpenInEditor",
		).WithReadOnly("ListProjects", "GetProject", "GetProjectRawType", "ListWorktrees").
			// AdoptProject accepts a caller-named filesystem path and registers
			// it as ClonePath, which DeleteProject later os.RemoveAll's — unlike
			// every other registration path, whose ClonePath is server-derived.
			WithLocalOnly("OpenInTerminal", "OpenInEditor", "AdoptProject"),
		"IntegrationService": httpapi.NewService(a.intgSvc,
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
		).WithReadOnly(
			"FetchRenovatePRs",
			"FetchAssignedIssues",
			"GetProviderHealth",
			"ProviderHealthEnabled",
		),
		"LoopAgentService": httpapi.NewService(a.loopAgentSvc,
			"ListLoopAgents",
			"GetLoopAgent",
			"CreateLoopAgent",
			"UpdateLoopAgent",
			"DeleteLoopAgent",
			"RunLoopAgentNow",
			"ListLoopAgentRuns",
		).WithReadOnly("ListLoopAgents", "GetLoopAgent", "ListLoopAgentRuns"),
	}
}

// LocalBrowserServices is the registry a UI attached to a board on another
// machine serves locally. Opening a window or a link acts on the machine the
// operator is sitting at, so those calls stay here while every other call goes
// to the board.
func LocalBrowserServices(open func(string)) map[string]httpapi.Service {
	svc := &BrowserService{open: open}
	return map[string]httpapi.Service{
		"BrowserService": httpapi.NewService(svc, "Open", "OpenExternal").
			WithLocalOnly("Open", "OpenExternal"),
	}
}
