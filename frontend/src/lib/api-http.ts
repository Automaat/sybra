// HTTP fetch implementations of all Wails bound methods.
// Used by api.ts when VITE_MODE=web.

import type {
  agent,
  task,
  project,
  github,
  loopagent,
  synapse,
  notification,
  stats,
  workflow,
  todoist,
  provider,
} from '../../wailsjs/go/models.js'

const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

async function call<T>(service: string, method: string, ...args: unknown[]): Promise<T> {
  const res = await fetch(`${API_BASE}/${service}/${method}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: args.length > 0 ? JSON.stringify(args) : undefined,
  })
  if (!res.ok) throw new Error(await res.text())
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}

// AgentService
export function DiscoverAgents(): Promise<Array<agent.Agent>> { return call('AgentService', 'DiscoverAgents') }
export function GetAgentOutput(arg1: string): Promise<Array<agent.StreamEvent>> { return call('AgentService', 'GetAgentOutput', arg1) }
export function GetAgentRunLog(arg1: string, arg2: string): Promise<Array<agent.StreamEvent>> { return call('AgentService', 'GetAgentRunLog', arg1, arg2) }
export function GetConvoOutput(arg1: string): Promise<Array<agent.ConvoEvent>> { return call('AgentService', 'GetConvoOutput', arg1) }
export function ListAgents(): Promise<Array<agent.Agent>> { return call('AgentService', 'ListAgents') }
export function RespondApproval(arg1: string, arg2: boolean): Promise<void> { return call('AgentService', 'RespondApproval', arg1, arg2) }
export function RespondEscalation(arg1: string, arg2: boolean): Promise<void> { return call('AgentService', 'RespondEscalation', arg1, arg2) }
export function SendMessage(arg1: string, arg2: string): Promise<void> { return call('AgentService', 'SendMessage', arg1, arg2) }
export function StopAgent(arg1: string): Promise<void> { return call('AgentService', 'StopAgent', arg1) }

// App
export function GetMonitorReport(): Promise<synapse.MonitorReportBinding> { return call('App', 'GetMonitorReport') }
export function ListNotifications(): Promise<Array<notification.Notification>> { return call('App', 'ListNotifications') }
export function RegisterSpotlightHotkey(): Promise<void> { return call('App', 'RegisterSpotlightHotkey') }
export function SetDesktopNotifications(arg1: boolean): Promise<void> { return call('App', 'SetDesktopNotifications', arg1) }
export function StartAgent(arg1: string, arg2: string, arg3: string): Promise<agent.Agent> { return call('App', 'StartAgent', arg1, arg2, arg3) }
export function StartChat(arg1: string, arg2: string, arg3: string): Promise<agent.Agent> { return call('App', 'StartChat', arg1, arg2, arg3) }
export function StopChat(arg1: string): Promise<void> { return call('App', 'StopChat', arg1) }

// ConfigService
export function GetSettings(): Promise<synapse.AppSettings> { return call('ConfigService', 'GetSettings') }
export function UpdateSettings(arg1: synapse.AppSettings): Promise<void> { return call('ConfigService', 'UpdateSettings', arg1) }

// IntegrationService
export function ApproveRenovatePR(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'ApproveRenovatePR', arg1, arg2) }
export function FetchAssignedIssues(): Promise<Array<github.Issue>> { return call('IntegrationService', 'FetchAssignedIssues') }
export function FetchRenovatePRs(): Promise<Array<github.RenovatePR>> { return call('IntegrationService', 'FetchRenovatePRs') }
export function FixRenovateCI(arg1: string, arg2: number, arg3: string, arg4: string): Promise<void> { return call('IntegrationService', 'FixRenovateCI', arg1, arg2, arg3, arg4) }
export function GetProviderHealth(): Promise<Array<provider.Status>> { return call('IntegrationService', 'GetProviderHealth') }
export function GetTodoistProjects(): Promise<Array<todoist.Project>> { return call('IntegrationService', 'GetTodoistProjects') }
export function ProviderHealthEnabled(): Promise<boolean> { return call('IntegrationService', 'ProviderHealthEnabled') }
export function MergeRenovatePR(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'MergeRenovatePR', arg1, arg2) }
export function RerunRenovateChecks(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'RerunRenovateChecks', arg1, arg2) }
export function SyncTodoist(): Promise<void> { return call('IntegrationService', 'SyncTodoist') }
export function TodoistEnabled(): Promise<boolean> { return call('IntegrationService', 'TodoistEnabled') }

// LoopAgentService
export function CreateLoopAgent(arg1: loopagent.LoopAgent): Promise<loopagent.LoopAgent> { return call('LoopAgentService', 'CreateLoopAgent', arg1) }
export function DeleteLoopAgent(arg1: string): Promise<void> { return call('LoopAgentService', 'DeleteLoopAgent', arg1) }
export function GetLoopAgent(arg1: string): Promise<loopagent.LoopAgent> { return call('LoopAgentService', 'GetLoopAgent', arg1) }
export function ListLoopAgentRuns(arg1: string, arg2: number): Promise<Array<synapse.LoopAgentRun>> { return call('LoopAgentService', 'ListLoopAgentRuns', arg1, arg2) }
export function ListLoopAgents(): Promise<Array<loopagent.LoopAgent>> { return call('LoopAgentService', 'ListLoopAgents') }
export function RunLoopAgentNow(arg1: string): Promise<string> { return call('LoopAgentService', 'RunLoopAgentNow', arg1) }
export function UpdateLoopAgent(arg1: loopagent.LoopAgent): Promise<loopagent.LoopAgent> { return call('LoopAgentService', 'UpdateLoopAgent', arg1) }

// OrchestratorService
export function GetOrchestratorAgentID(): Promise<string> { return call('OrchestratorService', 'GetOrchestratorAgentID') }
export function IsOrchestratorRunning(): Promise<boolean> { return call('OrchestratorService', 'IsOrchestratorRunning') }
export function StartOrchestrator(): Promise<void> { return call('OrchestratorService', 'StartOrchestrator') }
export function StopOrchestrator(): Promise<void> { return call('OrchestratorService', 'StopOrchestrator') }

// PlanningService
export function ApprovePlan(arg1: string): Promise<task.Task> { return call('PlanningService', 'ApprovePlan', arg1) }
export function ApproveTestPlan(arg1: string): Promise<task.Task> { return call('PlanningService', 'ApproveTestPlan', arg1) }
export function HasLivePlanAgent(arg1: string): Promise<boolean> { return call('PlanningService', 'HasLivePlanAgent', arg1) }
export function HasLiveTestPlanAgent(arg1: string): Promise<boolean> { return call('PlanningService', 'HasLiveTestPlanAgent', arg1) }
export function PlanTask(arg1: string): Promise<void> { return call('PlanningService', 'PlanTask', arg1) }
export function RejectPlan(arg1: string, arg2: string): Promise<task.Task> { return call('PlanningService', 'RejectPlan', arg1, arg2) }
export function RejectTestPlan(arg1: string, arg2: string): Promise<task.Task> { return call('PlanningService', 'RejectTestPlan', arg1, arg2) }
export function SendPlanMessage(arg1: string, arg2: string): Promise<void> { return call('PlanningService', 'SendPlanMessage', arg1, arg2) }
export function SendTestPlanMessage(arg1: string, arg2: string): Promise<void> { return call('PlanningService', 'SendTestPlanMessage', arg1, arg2) }
export function TriageTask(arg1: string): Promise<void> { return call('PlanningService', 'TriageTask', arg1) }

// ProjectService
export function CreateProject(arg1: string, arg2: string): Promise<project.Project> { return call('ProjectService', 'CreateProject', arg1, arg2) }
export function DeleteProject(arg1: string): Promise<void> { return call('ProjectService', 'DeleteProject', arg1) }
export function GetProject(arg1: string): Promise<project.Project> { return call('ProjectService', 'GetProject', arg1) }
export function ListProjects(): Promise<Array<project.Project>> { return call('ProjectService', 'ListProjects') }
export function ListWorktrees(arg1: string): Promise<Array<project.Worktree>> { return call('ProjectService', 'ListWorktrees', arg1) }
export function OpenInEditor(arg1: string): Promise<void> { return call('ProjectService', 'OpenInEditor', arg1) }
export function OpenInTerminal(arg1: string): Promise<void> { return call('ProjectService', 'OpenInTerminal', arg1) }
export function UpdateProject(arg1: string, arg2: string): Promise<project.Project> { return call('ProjectService', 'UpdateProject', arg1, arg2) }

// ReviewService
export function AddReviewComment(arg1: string, arg2: number, arg3: string): Promise<task.ReviewComment> { return call('ReviewService', 'AddReviewComment', arg1, arg2, arg3) }
export function DeleteReviewComment(arg1: string, arg2: string): Promise<void> { return call('ReviewService', 'DeleteReviewComment', arg1, arg2) }
export function FetchReviews(): Promise<github.ReviewSummary> { return call('ReviewService', 'FetchReviews') }
export function ListReviewComments(arg1: string): Promise<Array<task.ReviewComment>> { return call('ReviewService', 'ListReviewComments', arg1) }
export function MarkPRReady(arg1: string, arg2: number): Promise<void> { return call('ReviewService', 'MarkPRReady', arg1, arg2) }
export function ResolveReviewComment(arg1: string, arg2: string): Promise<void> { return call('ReviewService', 'ResolveReviewComment', arg1, arg2) }
export function StartFixReview(arg1: string): Promise<void> { return call('ReviewService', 'StartFixReview', arg1) }
export function StartReview(arg1: string): Promise<void> { return call('ReviewService', 'StartReview', arg1) }

// StatsService
export function GetStats(): Promise<stats.StatsResponse> { return call('StatsService', 'GetStats') }

// TaskService
export function CreateTask(arg1: string, arg2: string, arg3: string): Promise<task.Task> { return call('TaskService', 'CreateTask', arg1, arg2, arg3) }
export function DeleteTask(arg1: string): Promise<void> { return call('TaskService', 'DeleteTask', arg1) }
export function GetTask(arg1: string): Promise<task.Task> { return call('TaskService', 'GetTask', arg1) }
export function ListTasks(): Promise<Array<task.Task>> { return call('TaskService', 'ListTasks') }
export function UpdateTask(arg1: string, arg2: Record<string, unknown>): Promise<task.Task> { return call('TaskService', 'UpdateTask', arg1, arg2) }

// WorkflowService
export function DeleteWorkflow(arg1: string): Promise<void> { return call('WorkflowService', 'DeleteWorkflow', arg1) }
export function GetWorkflow(arg1: string): Promise<workflow.Definition> { return call('WorkflowService', 'GetWorkflow', arg1) }
export function HandleHumanAction(arg1: string, arg2: string, arg3: Record<string, string>): Promise<void> { return call('WorkflowService', 'HandleHumanAction', arg1, arg2, arg3) }
export function ListWorkflows(): Promise<Array<workflow.Definition>> { return call('WorkflowService', 'ListWorkflows') }
export function ResetBuiltin(arg1: string): Promise<void> { return call('WorkflowService', 'ResetBuiltin', arg1) }
export function SaveWorkflow(arg1: workflow.Definition): Promise<void> { return call('WorkflowService', 'SaveWorkflow', arg1) }
export function StartWorkflow(arg1: string, arg2: string): Promise<void> { return call('WorkflowService', 'StartWorkflow', arg1, arg2) }

// Shared EventSource for the multiplexed /events SSE stream.
// All EventsOn subscriptions funnel through a single connection.
const EVENTS_URL = (() => {
  // Strip /api suffix to get server root, then append /events.
  const base = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'
  return base.replace(/\/api$/, '') + '/events'
})()

let _sharedES: EventSource | null = null
let _subCount = 0

function getSharedES(): EventSource {
  if (!_sharedES) {
    _sharedES = new EventSource(EVENTS_URL)
  }
  return _sharedES
}

// Runtime: EventsOn via multiplexed SSE stream (GET /events).
// All subscriptions share a single EventSource connection.
// The server uses SSE named-event format so each listener only fires for its event.
export function EventsOn(eventName: string, callback: (...data: any[]) => void): () => void {
  const es = getSharedES()
  _subCount++

  const handler = (e: MessageEvent) => {
    try { callback(JSON.parse(e.data as string)) } catch { callback(e.data) }
  }
  es.addEventListener(eventName, handler)

  return () => {
    _sharedES?.removeEventListener(eventName, handler)
    _subCount--
    if (_subCount === 0) {
      _sharedES?.close()
      _sharedES = null
    }
  }
}

// Runtime: BrowserOpenURL via window.open
export function BrowserOpenURL(url: string): void {
  window.open(url, '_blank')
}
