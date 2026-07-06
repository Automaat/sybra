// HTTP fetch implementations of all Wails bound methods.
// Used by api.ts when VITE_MODE=web.

import type { Agent, ConvoEvent, StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import type { ReviewComment, Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import type { Project, Worktree } from '../../bindings/github.com/Automaat/sybra/internal/project/models.js'
import type { Issue, RenovatePR, ReviewSummary } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'
import type { LoopAgent } from '../../bindings/github.com/Automaat/sybra/internal/loopagent/models.js'
import type { AppSettings, CodexModel, CopilotModel, LoopAgentRun, MonitorReportBinding, TamperReportDTO, VersionInfo } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
import type { Notification } from '../../bindings/github.com/Automaat/sybra/internal/notification/models.js'
import type { StatsResponse } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'
import type { Report as EvaluationReportData, PhaseReport as PhaseReportData } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'
import type { Definition } from '../../bindings/github.com/Automaat/sybra/internal/workflow/models.js'
import type { Project as TodoistProject } from '../../bindings/github.com/Automaat/sybra/internal/todoist/models.js'
import type { Status } from '../../bindings/github.com/Automaat/sybra/internal/provider/models.js'
import type { Digest, Status as LearningDigestStatus } from '../../bindings/github.com/Automaat/sybra/internal/learning/models.js'

const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

// sybra-server gates every request (except GET /health) behind a shared
// bearer token (see cmd/sybra-server authMiddleware). A build-time default
// can be baked in via VITE_API_TOKEN; otherwise the token is entered once at
// runtime and cached in localStorage so it survives reloads.
const TOKEN_STORAGE_KEY = 'sybra.apiToken'

export function getApiToken(): string {
  if (typeof localStorage === 'undefined') return (import.meta.env.VITE_API_TOKEN as string | undefined) ?? ''
  return localStorage.getItem(TOKEN_STORAGE_KEY) || (import.meta.env.VITE_API_TOKEN as string | undefined) || ''
}

export function setApiToken(token: string): void {
  if (typeof localStorage === 'undefined') return
  localStorage.setItem(TOKEN_STORAGE_KEY, token)
}

// promptForApiToken asks the operator for the sybra-server auth token once
// per session (retrieved via `cat ~/.sybra/config.yaml` on the server host,
// key `server.auth_token`). Returns '' if the user cancels.
function promptForApiToken(): string {
  if (typeof window === 'undefined') return ''
  const entered = window.prompt('Sybra server auth token required (see server.auth_token in config.yaml):')
  if (!entered) return ''
  setApiToken(entered)
  return entered
}

async function call<T>(service: string, method: string, ...args: unknown[]): Promise<T> {
  const doFetch = () => fetch(`${API_BASE}/${service}/${method}`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(getApiToken() ? { Authorization: `Bearer ${getApiToken()}` } : {}),
    },
    body: args.length > 0 ? JSON.stringify(args) : undefined,
  })

  let res = await doFetch()
  if (res.status === 401 && promptForApiToken()) {
    res = await doFetch()
  }
  if (!res.ok) {
    const rawText = await res.text()
    // web-mode only: desktop Wails IPC errors never reach this path
    let parsed: unknown = null
    try { parsed = JSON.parse(rawText) } catch { /* fall through to rawText */ }
    if (parsed && typeof parsed === 'object' && 'error' in parsed && typeof (parsed as Record<string, unknown>).error === 'string') {
      const p = parsed as { error: string; code?: string }
      throw Object.assign(new Error(p.error), { code: p.code })
    }
    throw new Error(rawText)
  }
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}

// AgentService
export function DiscoverAgents(): Promise<Array<Agent>> { return call('AgentService', 'DiscoverAgents') }
export function GetAgentDiff(arg1: string): Promise<string> { return call('AgentService', 'GetAgentDiff', arg1) }
export function GetAgentOutput(arg1: string): Promise<Array<StreamEvent>> { return call('AgentService', 'GetAgentOutput', arg1) }
export function GetAgentRunConvoLog(arg1: string, arg2: string): Promise<Array<ConvoEvent>> { return call('AgentService', 'GetAgentRunConvoLog', arg1, arg2) }
export function GetAgentRunLog(arg1: string, arg2: string): Promise<Array<StreamEvent>> { return call('AgentService', 'GetAgentRunLog', arg1, arg2) }
export function GetConvoOutput(arg1: string): Promise<Array<ConvoEvent>> { return call('AgentService', 'GetConvoOutput', arg1) }
export function ListAgents(): Promise<Array<Agent>> { return call('AgentService', 'ListAgents') }
export function RespondApproval(arg1: string, arg2: boolean): Promise<void> { return call('AgentService', 'RespondApproval', arg1, arg2) }
export function RespondEscalation(arg1: string, arg2: boolean): Promise<void> { return call('AgentService', 'RespondEscalation', arg1, arg2) }
export function SendMessage(arg1: string, arg2: string): Promise<void> { return call('AgentService', 'SendMessage', arg1, arg2) }
export function StopAgent(arg1: string): Promise<void> { return call('AgentService', 'StopAgent', arg1) }
export function OpenWorktree(_arg1: string): Promise<void> { return Promise.reject(new Error('not available in web mode')) }

// App
export function GetMonitorReport(): Promise<MonitorReportBinding> { return call('App', 'GetMonitorReport') }
export function GetEvaluationReport(): Promise<EvaluationReportData> { return call('App', 'GetEvaluationReport') }
export function GetLifecyclePhases(): Promise<PhaseReportData> { return call('App', 'GetLifecyclePhases') }
export function GetLearningDigestStatus(): Promise<LearningDigestStatus> { return call('App', 'GetLearningDigestStatus') }
export function RunLearningDigestNow(): Promise<Digest> { return Promise.reject(new Error('not available in web mode')) }
export function ListBackgroundOps(): Promise<Array<any>> { return call('App', 'ListBackgroundOps') }
export function ListNotifications(): Promise<Array<Notification>> { return call('App', 'ListNotifications') }
export function RegisterSpotlightHotkey(): Promise<void> { return Promise.reject(new Error('not available in web mode')) }
export function SetDesktopNotifications(arg1: boolean): Promise<void> { return call('App', 'SetDesktopNotifications', arg1) }
export function StartAgent(arg1: string, arg2: string, arg3: string, arg4: boolean): Promise<Agent> { return call('App', 'StartAgent', arg1, arg2, arg3, arg4) }
export function StartChat(arg1: string, arg2: string, arg3: string): Promise<Agent> { return call('App', 'StartChat', arg1, arg2, arg3) }
export function StopChat(arg1: string): Promise<void> { return call('App', 'StopChat', arg1) }

// ConfigService
export function GetSettings(): Promise<AppSettings> { return call('ConfigService', 'GetSettings') }
export function GetDefaultSettings(): Promise<AppSettings> { return call('ConfigService', 'GetDefaultSettings') }
export function UpdateSettings(arg1: AppSettings): Promise<void> { return call('ConfigService', 'UpdateSettings', arg1) }
export function UpdateTodoistToken(arg1: string): Promise<void> { return call('ConfigService', 'UpdateTodoistToken', arg1) }
export function GetRawConfig(): Promise<string> { return call('ConfigService', 'GetRawConfig') }
export function SaveRawConfig(arg1: string): Promise<void> { return call('ConfigService', 'SaveRawConfig', arg1) }

// InfoService
export function GetCodexModels(): Promise<CodexModel[]> { return call('InfoService', 'GetCodexModels') }
export function GetCopilotModels(): Promise<CopilotModel[]> { return call('InfoService', 'GetCopilotModels') }
export function GetVersion(): Promise<VersionInfo> { return call('InfoService', 'GetVersion') }

// IntegrationService
export function ApproveRenovatePR(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'ApproveRenovatePR', arg1, arg2) }
export function FetchAssignedIssues(): Promise<Array<Issue>> { return call('IntegrationService', 'FetchAssignedIssues') }
export function FetchRenovatePRs(): Promise<Array<RenovatePR>> { return call('IntegrationService', 'FetchRenovatePRs') }
export function FixRenovateCI(arg1: string, arg2: number, arg3: string, arg4: string): Promise<void> { return call('IntegrationService', 'FixRenovateCI', arg1, arg2, arg3, arg4) }
export function GetProviderHealth(): Promise<Array<Status>> { return call('IntegrationService', 'GetProviderHealth') }
export function GetTodoistProjects(): Promise<Array<TodoistProject>> { return call('IntegrationService', 'GetTodoistProjects') }
export function ProviderHealthEnabled(): Promise<boolean> { return call('IntegrationService', 'ProviderHealthEnabled') }
export function MergeRenovatePR(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'MergeRenovatePR', arg1, arg2) }
export function RerunRenovateChecks(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'RerunRenovateChecks', arg1, arg2) }
export function SetProviderAutoFailover(arg1: boolean): Promise<void> { return call('IntegrationService', 'SetProviderAutoFailover', arg1) }
export function SetProviderEnabled(arg1: string, arg2: boolean): Promise<void> { return call('IntegrationService', 'SetProviderEnabled', arg1, arg2) }
export function SyncTodoist(): Promise<void> { return call('IntegrationService', 'SyncTodoist') }
export function TodoistEnabled(): Promise<boolean> { return call('IntegrationService', 'TodoistEnabled') }

// LoopAgentService
export function CreateLoopAgent(arg1: LoopAgent): Promise<LoopAgent> { return call('LoopAgentService', 'CreateLoopAgent', arg1) }
export function DeleteLoopAgent(arg1: string): Promise<void> { return call('LoopAgentService', 'DeleteLoopAgent', arg1) }
export function GetLoopAgent(arg1: string): Promise<LoopAgent> { return call('LoopAgentService', 'GetLoopAgent', arg1) }
export function ListLoopAgentRuns(arg1: string, arg2: number): Promise<Array<LoopAgentRun>> { return call('LoopAgentService', 'ListLoopAgentRuns', arg1, arg2) }
export function ListLoopAgents(): Promise<Array<LoopAgent>> { return call('LoopAgentService', 'ListLoopAgents') }
export function RunLoopAgentNow(arg1: string): Promise<string> { return call('LoopAgentService', 'RunLoopAgentNow', arg1) }
export function UpdateLoopAgent(arg1: LoopAgent): Promise<LoopAgent> { return call('LoopAgentService', 'UpdateLoopAgent', arg1) }

// OrchestratorService
export function GetOrchestratorAgentID(): Promise<string> { return call('OrchestratorService', 'GetOrchestratorAgentID') }
export function IsOrchestratorRunning(): Promise<boolean> { return call('OrchestratorService', 'IsOrchestratorRunning') }
export function StartOrchestrator(): Promise<void> { return call('OrchestratorService', 'StartOrchestrator') }
export function StopOrchestrator(): Promise<void> { return call('OrchestratorService', 'StopOrchestrator') }

// PlanningService
export function ApprovePlan(arg1: string): Promise<Task> { return call('PlanningService', 'ApprovePlan', arg1) }
export function HasLivePlanAgent(arg1: string): Promise<boolean> { return call('PlanningService', 'HasLivePlanAgent', arg1) }
export function PlanTask(arg1: string): Promise<void> { return call('PlanningService', 'PlanTask', arg1) }
export function RejectPlan(arg1: string, arg2: string): Promise<Task> { return call('PlanningService', 'RejectPlan', arg1, arg2) }
export function SendPlanMessage(arg1: string, arg2: string): Promise<void> { return call('PlanningService', 'SendPlanMessage', arg1, arg2) }
export function TriageTask(arg1: string): Promise<void> { return call('PlanningService', 'TriageTask', arg1) }

// ProjectService
export function CreateProject(arg1: string, arg2: string): Promise<Project> { return call('ProjectService', 'CreateProject', arg1, arg2) }
export function DeleteProject(arg1: string): Promise<void> { return call('ProjectService', 'DeleteProject', arg1) }
export function GetProject(arg1: string): Promise<Project> { return call('ProjectService', 'GetProject', arg1) }
export function ListProjects(): Promise<Array<Project>> { return call('ProjectService', 'ListProjects') }
export function ListWorktrees(arg1: string): Promise<Array<Worktree>> { return call('ProjectService', 'ListWorktrees', arg1) }
export function OpenInEditor(_arg1: string): Promise<void> { return Promise.reject(new Error('not available in web mode')) }
export function OpenInTerminal(_arg1: string): Promise<void> { return Promise.reject(new Error('not available in web mode')) }
export function SetProjectWorktreeBaseRef(arg1: string, arg2: string): Promise<Project> { return call('ProjectService', 'SetProjectWorktreeBaseRef', arg1, arg2) }
export function UpdateProject(arg1: string, arg2: string): Promise<Project> { return call('ProjectService', 'UpdateProject', arg1, arg2) }

// ReviewService
export function AddReviewComment(arg1: string, arg2: number, arg3: string): Promise<ReviewComment> { return call('ReviewService', 'AddReviewComment', arg1, arg2, arg3) }
export function DeleteReviewComment(arg1: string, arg2: string): Promise<void> { return call('ReviewService', 'DeleteReviewComment', arg1, arg2) }
export function FetchReviews(): Promise<ReviewSummary> { return call('ReviewService', 'FetchReviews') }
export function ListReviewComments(arg1: string): Promise<Array<ReviewComment>> { return call('ReviewService', 'ListReviewComments', arg1) }
export function MarkPRReady(arg1: string, arg2: number): Promise<void> { return call('ReviewService', 'MarkPRReady', arg1, arg2) }
export function ResolveReviewComment(arg1: string, arg2: string): Promise<void> { return call('ReviewService', 'ResolveReviewComment', arg1, arg2) }
export function StartFixReview(arg1: string): Promise<void> { return call('ReviewService', 'StartFixReview', arg1) }
export function StartReview(arg1: string): Promise<void> { return call('ReviewService', 'StartReview', arg1) }

// StatsService
export function GetStats(): Promise<StatsResponse> { return call('StatsService', 'GetStats') }

// LearningService
export function ListDigests(): Promise<Array<Digest>> { return call('LearningService', 'ListDigests') }
export function GetLatestDigest(): Promise<[Digest, boolean]> { return call('LearningService', 'GetLatestDigest') }

// TaskService
export function BlessTampering(arg1: string): Promise<Task> { return call('TaskService', 'BlessTampering', arg1) }
export function CreateTask(arg1: string, arg2: string, arg3: string): Promise<Task> { return call('TaskService', 'CreateTask', arg1, arg2, arg3) }
export function DeleteTask(arg1: string): Promise<void> { return call('TaskService', 'DeleteTask', arg1) }
export function GetTamperReport(arg1: string): Promise<TamperReportDTO> { return call('TaskService', 'GetTamperReport', arg1) }
export function GetTask(arg1: string): Promise<Task> { return call('TaskService', 'GetTask', arg1) }
export function ListTasks(): Promise<Array<Task>> { return call('TaskService', 'ListTasks') }
export function UpdateTask(arg1: string, arg2: Record<string, unknown>): Promise<Task> { return call('TaskService', 'UpdateTask', arg1, arg2) }

// WorkflowService
export function DeleteWorkflow(arg1: string): Promise<void> { return call('WorkflowService', 'DeleteWorkflow', arg1) }
export function GetWorkflow(arg1: string): Promise<Definition> { return call('WorkflowService', 'GetWorkflow', arg1) }
export function HandleHumanAction(arg1: string, arg2: string, arg3: Record<string, string>): Promise<void> { return call('WorkflowService', 'HandleHumanAction', arg1, arg2, arg3) }
export function ListWorkflows(): Promise<Array<Definition>> { return call('WorkflowService', 'ListWorkflows') }
export function ResetBuiltin(arg1: string): Promise<void> { return call('WorkflowService', 'ResetBuiltin', arg1) }
export function SaveWorkflow(arg1: Definition): Promise<void> { return call('WorkflowService', 'SaveWorkflow', arg1) }
export function StartWorkflow(arg1: string, arg2: string): Promise<void> { return call('WorkflowService', 'StartWorkflow', arg1, arg2) }

// Shared EventSource for the multiplexed /events SSE stream.
// All EventsOn subscriptions funnel through a single connection.
// EventSource cannot set an Authorization header, so the token travels as a
// query param instead — the server's authMiddleware accepts either form, but
// only for SSE paths (see isSSEPath in cmd/sybra-server).
function eventsURL(): string {
  // Strip /api suffix to get server root, then append /events.
  const base = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'
  const url = base.replace(/\/api$/, '') + '/events'
  const token = getApiToken()
  return token ? `${url}?token=${encodeURIComponent(token)}` : url
}

let _sharedES: EventSource | null = null
let _subCount = 0

function getSharedES(): EventSource {
  if (!_sharedES) {
    _sharedES = new EventSource(eventsURL())
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
