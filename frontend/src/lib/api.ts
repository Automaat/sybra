// Transport shim: VITE_MODE=desktop (default) → Wails IPC, VITE_MODE=web → HTTP fetch.
// All source files import from here instead of bindings/* directly.
//
// Desktop path uses Wails v3 alpha bindings (frontend/bindings/...) generated
// by `wails3 generate bindings ./cmd/sybra-v3/...`.

import * as AgentSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/agentservice.js'
import * as AppSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/app.js'
import * as BrowserSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/browserservice.js'
import * as ClusterSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/clusterservice.js'
import * as ConfigSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/configservice.js'
import * as IntegrationSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/integrationservice.js'
import * as LearningSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/learningservice.js'
import * as LoopSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/loopagentservice.js'
import * as OrchestratorSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/orchestratorservice.js'
import * as PlanningSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/planningservice.js'
import * as PromptLabSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/promptlabservice.js'
import * as ProjectSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/projectservice.js'
import * as ReviewSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/reviewservice.js'
import * as StatsSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/statsservice.js'
import * as TaskSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/taskservice.js'
import * as InfoSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/infoservice.js'
import * as WorkflowSvc from '../../bindings/github.com/Automaat/sybra/internal/sybra/workflowservice.js'
import { Events as _Events, Browser as _Browser } from '@wailsio/runtime'
import * as http from './api-http.js'

// pick selects the desktop or web implementation at module init time.
// import.meta.env.VITE_MODE is a build-time constant; Vite tree-shakes the dead branch.
//
// Desktop type is `unknown` because v3-generated bindings differ from the
// HTTP shim in two superficial ways:
//   1. Pointer returns are `T | null` instead of `T`.
//   2. Promises are `CancellablePromise<T>` instead of `Promise<T>`.
// Both reduce to the http shape at runtime; we let the http signature drive
// the public type so call-sites stay v2-compatible.
function pick<T>(desktop: unknown, web: T): T {
  return (import.meta.env.VITE_MODE === 'web' ? web : (desktop as T))
}

// AgentService
export const DiscoverAgents = pick(AgentSvc.DiscoverAgents, http.DiscoverAgents)
export const GetAgentDiff = pick(AgentSvc.GetAgentDiff, http.GetAgentDiff)
export const GetAgentOutput = pick(AgentSvc.GetAgentOutput, http.GetAgentOutput)
export const GetAgentRunConvoLog = pick(AgentSvc.GetAgentRunConvoLog, http.GetAgentRunConvoLog)
export const GetAgentRunLog = pick(AgentSvc.GetAgentRunLog, http.GetAgentRunLog)
export const GetConvoOutput = pick(AgentSvc.GetConvoOutput, http.GetConvoOutput)
export const ListAgents = pick(AgentSvc.ListAgents, http.ListAgents)
export const RespondApproval = pick(AgentSvc.RespondApproval, http.RespondApproval)
export const RespondEscalation = pick(AgentSvc.RespondEscalation, http.RespondEscalation)
export const SendMessage = pick(AgentSvc.SendMessage, http.SendMessage)
export const StopAgent = pick(AgentSvc.StopAgent, http.StopAgent)
export const OpenWorktree = pick(AgentSvc.OpenWorktree, http.OpenWorktree)

// App
export const GetMonitorReport = pick(AppSvc.GetMonitorReport, http.GetMonitorReport)
export const GetEvaluationReport = pick(AppSvc.GetEvaluationReport, http.GetEvaluationReport)
export const GetLifecyclePhases = pick(AppSvc.GetLifecyclePhases, http.GetLifecyclePhases)
export const GetLearningDigestStatus = pick(AppSvc.GetLearningDigestStatus, http.GetLearningDigestStatus)
export const RunLearningDigestNow = pick(AppSvc.RunLearningDigestNow, http.RunLearningDigestNow)
export const ListBackgroundOps = pick(AppSvc.ListBackgroundOps, http.ListBackgroundOps)
export const ListNotifications = pick(AppSvc.ListNotifications, http.ListNotifications)
export const RegisterSpotlightHotkey = pick(AppSvc.RegisterSpotlightHotkey, http.RegisterSpotlightHotkey)
export const SetDesktopNotifications = pick(AppSvc.SetDesktopNotifications, http.SetDesktopNotifications)
export const StartAgent = pick(AppSvc.StartAgent, http.StartAgent)
export const StartChat = pick(AppSvc.StartChat, http.StartChat)
export const StopChat = pick(AppSvc.StopChat, http.StopChat)
export const AgentQueueSnapshot = pick(AppSvc.AgentQueueSnapshot, http.AgentQueueSnapshot)

// ConfigService
export const GetSettings = pick(ConfigSvc.GetSettings, http.GetSettings)
export const GetDefaultSettings = pick(ConfigSvc.GetDefaultSettings, http.GetDefaultSettings)
export const UpdateSettings = pick(ConfigSvc.UpdateSettings, http.UpdateSettings)
export const UpdateTodoistToken = pick(ConfigSvc.UpdateTodoistToken, http.UpdateTodoistToken)
export const GetRawConfig = pick(ConfigSvc.GetRawConfig, http.GetRawConfig)
export const SaveRawConfig = pick(ConfigSvc.SaveRawConfig, http.SaveRawConfig)

// InfoService
export const GetAvailableRuntimes = pick(InfoSvc.GetAvailableRuntimes, http.GetAvailableRuntimes)
export const GetCodexModels = pick(InfoSvc.GetCodexModels, http.GetCodexModels)
export const GetCopilotModels = pick(InfoSvc.GetCopilotModels, http.GetCopilotModels)
export const GetVersion = pick(InfoSvc.GetVersion, http.GetVersion)

// IntegrationService
export const ApproveRenovatePR = pick(IntegrationSvc.ApproveRenovatePR, http.ApproveRenovatePR)
export const FetchAssignedIssues = pick(IntegrationSvc.FetchAssignedIssues, http.FetchAssignedIssues)
export const FetchRenovatePRs = pick(IntegrationSvc.FetchRenovatePRs, http.FetchRenovatePRs)
export const FixRenovateCI = pick(IntegrationSvc.FixRenovateCI, http.FixRenovateCI)
export const GetProviderHealth = pick(IntegrationSvc.GetProviderHealth, http.GetProviderHealth)
export const GetTodoistProjects = pick(IntegrationSvc.GetTodoistProjects, http.GetTodoistProjects)
export const MergeRenovatePR = pick(IntegrationSvc.MergeRenovatePR, http.MergeRenovatePR)
export const ProviderHealthEnabled = pick(IntegrationSvc.ProviderHealthEnabled, http.ProviderHealthEnabled)
export const RerunRenovateChecks = pick(IntegrationSvc.RerunRenovateChecks, http.RerunRenovateChecks)
export const SetProviderAutoFailover = pick(IntegrationSvc.SetProviderAutoFailover, http.SetProviderAutoFailover)
export const SetProviderEnabled = pick(IntegrationSvc.SetProviderEnabled, http.SetProviderEnabled)
export const SyncTodoist = pick(IntegrationSvc.SyncTodoist, http.SyncTodoist)
export const TodoistEnabled = pick(IntegrationSvc.TodoistEnabled, http.TodoistEnabled)

// LearningService
export const ListDigests = pick(LearningSvc.ListDigests, http.ListDigests)
export const GetLatestDigest = pick(LearningSvc.GetLatestDigest, http.GetLatestDigest)

// LoopAgentService
export const CreateLoopAgent = pick(LoopSvc.CreateLoopAgent, http.CreateLoopAgent)
export const DeleteLoopAgent = pick(LoopSvc.DeleteLoopAgent, http.DeleteLoopAgent)
export const GetLoopAgent = pick(LoopSvc.GetLoopAgent, http.GetLoopAgent)
export const ListLoopAgentRuns = pick(LoopSvc.ListLoopAgentRuns, http.ListLoopAgentRuns)
export const ListLoopAgents = pick(LoopSvc.ListLoopAgents, http.ListLoopAgents)
export const RunLoopAgentNow = pick(LoopSvc.RunLoopAgentNow, http.RunLoopAgentNow)
export const UpdateLoopAgent = pick(LoopSvc.UpdateLoopAgent, http.UpdateLoopAgent)

// OrchestratorService
export const GetOrchestratorAgentID = pick(OrchestratorSvc.GetOrchestratorAgentID, http.GetOrchestratorAgentID)
export const IsOrchestratorRunning = pick(OrchestratorSvc.IsOrchestratorRunning, http.IsOrchestratorRunning)
export const StartOrchestrator = pick(OrchestratorSvc.StartOrchestrator, http.StartOrchestrator)
export const StopOrchestrator = pick(OrchestratorSvc.StopOrchestrator, http.StopOrchestrator)

// PlanningService
export const ApprovePlan = pick(PlanningSvc.ApprovePlan, http.ApprovePlan)
export const HasLivePlanAgent = pick(PlanningSvc.HasLivePlanAgent, http.HasLivePlanAgent)
export const PlanTask = pick(PlanningSvc.PlanTask, http.PlanTask)
export const RejectPlan = pick(PlanningSvc.RejectPlan, http.RejectPlan)
export const SendPlanMessage = pick(PlanningSvc.SendPlanMessage, http.SendPlanMessage)
export const TriageTask = pick(PlanningSvc.TriageTask, http.TriageTask)

// PromptLabService
export const ApproveProposal = pick(PromptLabSvc.ApproveProposal, http.ApproveProposal)
export const RejectProposal = pick(PromptLabSvc.RejectProposal, http.RejectProposal)

// ProjectService
export const CreateProject = pick(ProjectSvc.CreateProject, http.CreateProject)
export const DeleteProject = pick(ProjectSvc.DeleteProject, http.DeleteProject)
export const GetProject = pick(ProjectSvc.GetProject, http.GetProject)
export const ListProjects = pick(ProjectSvc.ListProjects, http.ListProjects)
export const ListWorktrees = pick(ProjectSvc.ListWorktrees, http.ListWorktrees)
export const OpenInEditor = pick(ProjectSvc.OpenInEditor, http.OpenInEditor)
export const OpenInTerminal = pick(ProjectSvc.OpenInTerminal, http.OpenInTerminal)
export const SetProjectWorktreeBaseRef = pick(ProjectSvc.SetProjectWorktreeBaseRef, http.SetProjectWorktreeBaseRef)
export const UpdateProject = pick(ProjectSvc.UpdateProject, http.UpdateProject)

// ReviewService
export const AddReviewComment = pick(ReviewSvc.AddReviewComment, http.AddReviewComment)
export const DeleteReviewComment = pick(ReviewSvc.DeleteReviewComment, http.DeleteReviewComment)
export const FetchReviews = pick(ReviewSvc.FetchReviews, http.FetchReviews)
export const ListReviewComments = pick(ReviewSvc.ListReviewComments, http.ListReviewComments)
export const MarkPRReady = pick(ReviewSvc.MarkPRReady, http.MarkPRReady)
export const ResolveReviewComment = pick(ReviewSvc.ResolveReviewComment, http.ResolveReviewComment)
export const StartFixReview = pick(ReviewSvc.StartFixReview, http.StartFixReview)
export const StartReview = pick(ReviewSvc.StartReview, http.StartReview)

// StatsService
export const GetStats = pick(StatsSvc.GetStats, http.GetStats)

// TaskService
export const BlessTampering = pick(TaskSvc.BlessTampering, http.BlessTampering)
export const CreateTask = pick(TaskSvc.CreateTask, http.CreateTask)
export const DeleteTask = pick(TaskSvc.DeleteTask, http.DeleteTask)
export const DispatchFromHumanRequired = pick(TaskSvc.DispatchFromHumanRequired, http.DispatchFromHumanRequired)
export const ListTaskArtifacts = pick(TaskSvc.ListTaskArtifacts, http.ListTaskArtifacts)
export const GetTaskSetupLog = pick(TaskSvc.GetTaskSetupLog, http.GetTaskSetupLog)
export const ListTaskAuditEvents = pick(TaskSvc.ListTaskAuditEvents, http.ListTaskAuditEvents)
export const GetTamperReport = pick(TaskSvc.GetTamperReport, http.GetTamperReport)
export const GetTask = pick(TaskSvc.GetTask, http.GetTask)
export const ListTasks = pick(TaskSvc.ListTasks, http.ListTasks)
export const ListTaskProgress = pick(TaskSvc.ListTaskProgress, http.ListTaskProgress)
export const UpdateTask = pick(TaskSvc.UpdateTask, http.UpdateTask)
export const AssignTask = pick(TaskSvc.AssignTask, http.AssignTask)
export const RecoverLostAgent = pick(TaskSvc.RecoverLostAgent, http.RecoverLostAgent)

// WorkflowService
export const DeleteWorkflow = pick(WorkflowSvc.DeleteWorkflow, http.DeleteWorkflow)
export const GetWorkflow = pick(WorkflowSvc.GetWorkflow, http.GetWorkflow)
export const HandleHumanAction = pick(WorkflowSvc.HandleHumanAction, http.HandleHumanAction)
export const ListWorkflows = pick(WorkflowSvc.ListWorkflows, http.ListWorkflows)
export const ResetBuiltin = pick(WorkflowSvc.ResetBuiltin, http.ResetBuiltin)
export const SaveWorkflow = pick(WorkflowSvc.SaveWorkflow, http.SaveWorkflow)
export const StartWorkflow = pick(WorkflowSvc.StartWorkflow, http.StartWorkflow)

// ClusterService
export const GetNodes = pick(ClusterSvc.GetNodes, http.GetNodes)
export const StopAgentOnNode = pick(ClusterSvc.StopAgentOnNode, http.StopAgentOnNode)
export const SendMessageToNode = pick(ClusterSvc.SendMessageToNode, http.SendMessageToNode)
export const RespondApprovalOnNode = pick(ClusterSvc.RespondApprovalOnNode, http.RespondApprovalOnNode)
export const ApprovePlanOnNode = pick(ClusterSvc.ApprovePlanOnNode, http.ApprovePlanOnNode)
export const RejectPlanOnNode = pick(ClusterSvc.RejectPlanOnNode, http.RejectPlanOnNode)
export const ListNodeAgents = pick(ClusterSvc.ListNodeAgents, http.ListNodeAgents)
export const GetAgentOutputOnNode = pick(ClusterSvc.GetAgentOutputOnNode, http.GetAgentOutputOnNode)
export const GetConvoOutputOnNode = pick(ClusterSvc.GetConvoOutputOnNode, http.GetConvoOutputOnNode)
export const ReassignTask = pick(ClusterSvc.ReassignTask, http.ReassignTask)

// Runtime events and browser utilities.
//
// v3 Events.On callback receives a `WailsEvent` object whose payload is on
// `event.data`; v2 EventsOn passed the raw value(s) as variadic args. The
// adapter below preserves the v2 call shape so existing Svelte stores keep
// working without per-callsite changes.
const _dEventsOn = (eventName: string, callback: (...args: unknown[]) => void): (() => void) => {
  return _Events.On(eventName, (event: { data: unknown }) => {
    if (Array.isArray(event.data)) {
      callback(...event.data)
    } else {
      callback(event.data)
    }
  })
}
const _dBrowserOpenURL = (url: string): void => {
  void _Browser.OpenURL(url)
}

export const EventsOn = pick(_dEventsOn, http.EventsOn)
export const BrowserOpenURL = pick(_dBrowserOpenURL, http.BrowserOpenURL)
// Desktop opens the URL in an in-app webview window; the web build has no native
// window, so it falls back to a new browser tab (http.BrowserOpenURL).
const _dOpenInAppBrowser = (url: string): void => {
  // A non-http(s) or malformed URL rejects in Go; fall back to the system
  // browser (the prior behavior) so the click isn't silently swallowed.
  void BrowserSvc.Open(url).catch(() => _dBrowserOpenURL(url))
}
export const OpenInAppBrowser = pick(_dOpenInAppBrowser, http.BrowserOpenURL)
