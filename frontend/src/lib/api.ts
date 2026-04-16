// Transport shim: VITE_MODE=desktop (default) → Wails IPC, VITE_MODE=web → HTTP fetch.
// All source files import from here instead of wailsjs/* directly.

import * as AgentSvc from '../../wailsjs/go/sybra/AgentService.js'
import * as AppSvc from '../../wailsjs/go/sybra/App.js'
import * as ConfigSvc from '../../wailsjs/go/sybra/ConfigService.js'
import * as IntegrationSvc from '../../wailsjs/go/sybra/IntegrationService.js'
import * as LoopSvc from '../../wailsjs/go/sybra/LoopAgentService.js'
import * as OrchestratorSvc from '../../wailsjs/go/sybra/OrchestratorService.js'
import * as PlanningSvc from '../../wailsjs/go/sybra/PlanningService.js'
import * as ProjectSvc from '../../wailsjs/go/sybra/ProjectService.js'
import * as ReviewSvc from '../../wailsjs/go/sybra/ReviewService.js'
import * as StatsSvc from '../../wailsjs/go/sybra/StatsService.js'
import * as TaskSvc from '../../wailsjs/go/sybra/TaskService.js'
import * as InfoSvc from '../../wailsjs/go/sybra/InfoService.js'
import * as WorkflowSvc from '../../wailsjs/go/sybra/WorkflowService.js'
import { EventsOn as _dEventsOn, BrowserOpenURL as _dBrowserOpenURL } from '../../wailsjs/runtime/runtime.js'
import * as http from './api-http.js'

// pick selects the desktop or web implementation at module init time.
// import.meta.env.VITE_MODE is a build-time constant; Vite tree-shakes the dead branch.
function pick<T>(desktop: T, web: T): T {
  return (import.meta.env.VITE_MODE === 'web' ? web : desktop)
}

// AgentService
export const DiscoverAgents = pick(AgentSvc.DiscoverAgents, http.DiscoverAgents)
export const GetAgentOutput = pick(AgentSvc.GetAgentOutput, http.GetAgentOutput)
export const GetAgentRunConvoLog = pick(AgentSvc.GetAgentRunConvoLog, http.GetAgentRunConvoLog)
export const GetAgentRunLog = pick(AgentSvc.GetAgentRunLog, http.GetAgentRunLog)
export const GetConvoOutput = pick(AgentSvc.GetConvoOutput, http.GetConvoOutput)
export const ListAgents = pick(AgentSvc.ListAgents, http.ListAgents)
export const RespondApproval = pick(AgentSvc.RespondApproval, http.RespondApproval)
export const RespondEscalation = pick(AgentSvc.RespondEscalation, http.RespondEscalation)
export const SendMessage = pick(AgentSvc.SendMessage, http.SendMessage)
export const StopAgent = pick(AgentSvc.StopAgent, http.StopAgent)

// App
export const GetMonitorReport = pick(AppSvc.GetMonitorReport, http.GetMonitorReport)
export const ListBackgroundOps = pick(AppSvc.ListBackgroundOps, http.ListBackgroundOps)
export const ListNotifications = pick(AppSvc.ListNotifications, http.ListNotifications)
export const RegisterSpotlightHotkey = pick(AppSvc.RegisterSpotlightHotkey, http.RegisterSpotlightHotkey)
export const SetDesktopNotifications = pick(AppSvc.SetDesktopNotifications, http.SetDesktopNotifications)
export const StartAgent = pick(AppSvc.StartAgent, http.StartAgent)
export const StartChat = pick(AppSvc.StartChat, http.StartChat)
export const StopChat = pick(AppSvc.StopChat, http.StopChat)

// ConfigService
export const GetSettings = pick(ConfigSvc.GetSettings, http.GetSettings)
export const UpdateSettings = pick(ConfigSvc.UpdateSettings, http.UpdateSettings)

// InfoService
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
export const SyncTodoist = pick(IntegrationSvc.SyncTodoist, http.SyncTodoist)
export const TodoistEnabled = pick(IntegrationSvc.TodoistEnabled, http.TodoistEnabled)

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
export const ApproveTestPlan = pick(PlanningSvc.ApproveTestPlan, http.ApproveTestPlan)
export const HasLivePlanAgent = pick(PlanningSvc.HasLivePlanAgent, http.HasLivePlanAgent)
export const HasLiveTestPlanAgent = pick(PlanningSvc.HasLiveTestPlanAgent, http.HasLiveTestPlanAgent)
export const PlanTask = pick(PlanningSvc.PlanTask, http.PlanTask)
export const RejectPlan = pick(PlanningSvc.RejectPlan, http.RejectPlan)
export const RejectTestPlan = pick(PlanningSvc.RejectTestPlan, http.RejectTestPlan)
export const SendPlanMessage = pick(PlanningSvc.SendPlanMessage, http.SendPlanMessage)
export const SendTestPlanMessage = pick(PlanningSvc.SendTestPlanMessage, http.SendTestPlanMessage)
export const TriageTask = pick(PlanningSvc.TriageTask, http.TriageTask)

// ProjectService
export const CreateProject = pick(ProjectSvc.CreateProject, http.CreateProject)
export const DeleteProject = pick(ProjectSvc.DeleteProject, http.DeleteProject)
export const GetProject = pick(ProjectSvc.GetProject, http.GetProject)
export const ListProjects = pick(ProjectSvc.ListProjects, http.ListProjects)
export const ListWorktrees = pick(ProjectSvc.ListWorktrees, http.ListWorktrees)
export const OpenInEditor = pick(ProjectSvc.OpenInEditor, http.OpenInEditor)
export const OpenInTerminal = pick(ProjectSvc.OpenInTerminal, http.OpenInTerminal)
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
export const CreateTask = pick(TaskSvc.CreateTask, http.CreateTask)
export const DeleteTask = pick(TaskSvc.DeleteTask, http.DeleteTask)
export const GetTask = pick(TaskSvc.GetTask, http.GetTask)
export const ListTasks = pick(TaskSvc.ListTasks, http.ListTasks)
export const UpdateTask = pick(TaskSvc.UpdateTask, http.UpdateTask)

// WorkflowService
export const DeleteWorkflow = pick(WorkflowSvc.DeleteWorkflow, http.DeleteWorkflow)
export const GetWorkflow = pick(WorkflowSvc.GetWorkflow, http.GetWorkflow)
export const HandleHumanAction = pick(WorkflowSvc.HandleHumanAction, http.HandleHumanAction)
export const ListWorkflows = pick(WorkflowSvc.ListWorkflows, http.ListWorkflows)
export const ResetBuiltin = pick(WorkflowSvc.ResetBuiltin, http.ResetBuiltin)
export const SaveWorkflow = pick(WorkflowSvc.SaveWorkflow, http.SaveWorkflow)
export const StartWorkflow = pick(WorkflowSvc.StartWorkflow, http.StartWorkflow)

// Runtime events and browser utilities
export const EventsOn = pick(_dEventsOn, http.EventsOn)
export const BrowserOpenURL = pick(_dBrowserOpenURL, http.BrowserOpenURL)
