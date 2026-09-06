// HTTP implementations of every server method. This is the only transport:
// the desktop window loads the SPA from the app's own loopback server, so it
// reaches state the same way a browser pointed at sybra-server does.

import type { Agent, ConvoEvent, StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import type { ReviewComment, Task, TransitionIntent, TransitionResult, TrashEntry, Update } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import type { Project, Worktree } from '../../bindings/github.com/Automaat/sybra/internal/project/models.js'
import type { Issue, RenovatePR, ReviewSummary } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'
import type { LoopAgent } from '../../bindings/github.com/Automaat/sybra/internal/loopagent/models.js'
import type { AppSettings, ClusterNodeDTO, TriageResultDTO, TrashPruneReportDTO, UmbrellaExpandDTO, TaskHistoryEntryDTO, MapDuplicateIncidentsDTO, HarnessEvolutionRunDTO, PromptLabRunDTO, CodexModel, ConfigPathExplanation, CopilotModel, LoopAgentRun, MonitorReportBinding, RuntimeInfo, TamperReportDTO, TaskArtifactDTO, TaskAuditEventDTO, TaskSetupLogDTO, VersionInfo, AgentQueueSnapshot as AgentQueueSnapshotData } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
import type { Notification } from '../../bindings/github.com/Automaat/sybra/internal/notification/models.js'
import type { RemoteResultRecoveryReport } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
import type { StatsResponse } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'
import type { Meta, ProgressEntry } from '../../bindings/github.com/Automaat/sybra/internal/artifact/models.js'
import type { Event as AuditEvent, Query as AuditQuery } from '../../bindings/github.com/Automaat/sybra/internal/audit/models.js'
import type { Report as MonitorReport } from '../../bindings/github.com/Automaat/sybra/internal/monitor/models.js'
import type { Report as SelfMonitorReport, LedgerEntry } from '../../bindings/github.com/Automaat/sybra/internal/selfmonitor/models.js'
import type { Report as EvaluationReport } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'
import type { Report as EvaluationReportData, PhaseReport as PhaseReportData, AutonomyTrend } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'
import type { Definition } from '../../bindings/github.com/Automaat/sybra/internal/workflow/models.js'
import type { Status } from '../../bindings/github.com/Automaat/sybra/internal/provider/models.js'
import type { Digest, Status as LearningDigestStatus } from '../../bindings/github.com/Automaat/sybra/internal/learning/models.js'

// Runtime config served by the host that delivered this bundle. The desktop
// app writes it so its window authenticates without an operator prompt; a
// browser build never receives it and keeps the localStorage flow below.
type RuntimeConfig = { apiBase?: string; token?: string }

function runtimeConfig(): RuntimeConfig {
  if (typeof window === 'undefined') return {}
  return (window as unknown as { __SYBRA_RUNTIME__?: RuntimeConfig }).__SYBRA_RUNTIME__ ?? {}
}

function apiBase(): string {
  return runtimeConfig().apiBase || (import.meta.env.VITE_API_BASE as string | undefined) || '/api'
}

// The server gates every request (except GET /health) behind a shared bearer
// token (see internal/httpserve AuthMiddleware). A build-time default can be
// baked in via VITE_API_TOKEN; otherwise the token is entered once at runtime
// and cached in localStorage so it survives reloads.
const TOKEN_STORAGE_KEY = 'sybra.apiToken'

export function getApiToken(): string {
  const injected = runtimeConfig().token
  if (injected) return injected
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
  // An injected token came from the host that served this bundle; a 401 under
  // it is a real failure, and prompting would only collect a worse answer.
  if (runtimeConfig().token) return ''
  if (typeof window === 'undefined') return ''
  const entered = window.prompt('Sybra server auth token required (see server.auth_token in config.yaml):')
  if (!entered) return ''
  setApiToken(entered)
  return entered
}

const RETRY_AFTER_BUSY_MS = 750

function isReplayable(method: string): boolean {
  return method.startsWith('List') || method.startsWith('Get')
}

async function call<T>(service: string, method: string, ...args: unknown[]): Promise<T> {
  const doFetch = () => fetch(`${apiBase()}/${service}/${method}`, {
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
  if (res.status === 503 && isReplayable(method)) {
    await new Promise((resolve) => setTimeout(resolve, RETRY_AFTER_BUSY_MS))
    res = await doFetch()
  }
  if (!res.ok) {
    const rawText = await res.text()
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
export function GetAgent(arg1: string): Promise<Agent> { return call('AgentService', 'GetAgent', arg1) }
export function GetAgentOutput(arg1: string): Promise<Array<StreamEvent>> { return call('AgentService', 'GetAgentOutput', arg1) }
export function GetAgentRunConvoLog(arg1: string, arg2: string): Promise<Array<ConvoEvent>> { return call('AgentService', 'GetAgentRunConvoLog', arg1, arg2) }
export function GetAgentRunLog(arg1: string, arg2: string): Promise<Array<StreamEvent>> { return call('AgentService', 'GetAgentRunLog', arg1, arg2) }
export function GetConvoOutput(arg1: string): Promise<Array<ConvoEvent>> { return call('AgentService', 'GetConvoOutput', arg1) }
export function ListAgents(): Promise<Array<Agent>> { return call('AgentService', 'ListAgents') }
export function RespondApproval(arg1: string, arg2: boolean): Promise<void> { return call('AgentService', 'RespondApproval', arg1, arg2) }
export function RespondEscalation(arg1: string, arg2: boolean): Promise<void> { return call('AgentService', 'RespondEscalation', arg1, arg2) }
export function SendMessage(arg1: string, arg2: string): Promise<void> { return call('AgentService', 'SendMessage', arg1, arg2) }
export function StopAgent(arg1: string): Promise<void> { return call('AgentService', 'StopAgent', arg1) }
export function OpenWorktree(arg1: string): Promise<void> { return call('AgentService', 'OpenWorktree', arg1) }

// App
export function GetMonitorReport(): Promise<MonitorReportBinding> { return call('App', 'GetMonitorReport') }
export function GetEvaluationReport(): Promise<EvaluationReportData> { return call('App', 'GetEvaluationReport') }
export function GetLifecyclePhases(): Promise<PhaseReportData> { return call('App', 'GetLifecyclePhases') }
export function GetAutonomyTrend(): Promise<AutonomyTrend> { return call('App', 'GetAutonomyTrend') }
export function GetLearningDigestStatus(): Promise<LearningDigestStatus> { return call('App', 'GetLearningDigestStatus') }
export function ReconcileRemoteResults(apply = false, after = '', limit = 100): Promise<RemoteResultRecoveryReport> { return call('App', 'ReconcileRemoteResults', apply, after, limit) }
export function RunLearningDigestNow(): Promise<Digest> { return call('App', 'RunLearningDigestNow') }
export function ListBackgroundOps(): Promise<Array<any>> { return call('App', 'ListBackgroundOps') }
export function ListNotifications(): Promise<Array<Notification>> { return call('App', 'ListNotifications') }
export function RegisterSpotlightHotkey(): Promise<void> { return call('App', 'RegisterSpotlightHotkey') }
export function SetDesktopNotifications(arg1: boolean): Promise<void> { return call('App', 'SetDesktopNotifications', arg1) }
export function StartAgent(arg1: string, arg2: string, arg3: string, arg4: boolean): Promise<Agent> { return call('App', 'StartAgent', arg1, arg2, arg3, arg4) }
export function AgentQueueSnapshot(): Promise<AgentQueueSnapshotData> { return call('App', 'AgentQueueSnapshot') }
export function StartK8sPocAgent(arg1: string): Promise<Agent> { return call('App', 'StartK8sPocAgent', arg1) }

// ConfigService
export function GetSettings(): Promise<AppSettings> { return call('ConfigService', 'GetSettings') }
export function GetPathExplanations(): Promise<Array<ConfigPathExplanation>> { return call('ConfigService', 'GetPathExplanations') }
export function GetDefaultSettings(): Promise<AppSettings> { return call('ConfigService', 'GetDefaultSettings') }
export function UpdateSettings(arg1: AppSettings): Promise<void> { return call('ConfigService', 'UpdateSettings', arg1) }
export function GetRawConfig(): Promise<string> { return call('ConfigService', 'GetRawConfig') }
export function SaveRawConfig(arg1: string): Promise<void> { return call('ConfigService', 'SaveRawConfig', arg1) }

// InfoService
export function GetAvailableRuntimes(): Promise<RuntimeInfo[]> { return call('InfoService', 'GetAvailableRuntimes') }
export function GetCodexModels(): Promise<CodexModel[]> { return call('InfoService', 'GetCodexModels') }
export function GetCopilotModels(): Promise<CopilotModel[]> { return call('InfoService', 'GetCopilotModels') }
export function GetVersion(): Promise<VersionInfo> { return call('InfoService', 'GetVersion') }

// IntegrationService
export function ApproveRenovatePR(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'ApproveRenovatePR', arg1, arg2) }
export function FetchAssignedIssues(): Promise<Array<Issue>> { return call('IntegrationService', 'FetchAssignedIssues') }
export function FetchRenovatePRs(): Promise<Array<RenovatePR>> { return call('IntegrationService', 'FetchRenovatePRs') }
export function FixRenovateCI(arg1: string, arg2: number, arg3: string, arg4: string): Promise<void> { return call('IntegrationService', 'FixRenovateCI', arg1, arg2, arg3, arg4) }
export function GetProviderHealth(): Promise<Array<Status>> { return call('IntegrationService', 'GetProviderHealth') }
export function ProviderHealthEnabled(): Promise<boolean> { return call('IntegrationService', 'ProviderHealthEnabled') }
export function MergeRenovatePR(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'MergeRenovatePR', arg1, arg2) }
export function RerunRenovateChecks(arg1: string, arg2: number): Promise<void> { return call('IntegrationService', 'RerunRenovateChecks', arg1, arg2) }
export function SetProviderAutoFailover(arg1: boolean): Promise<void> { return call('IntegrationService', 'SetProviderAutoFailover', arg1) }
export function SetProviderEnabled(arg1: string, arg2: boolean): Promise<void> { return call('IntegrationService', 'SetProviderEnabled', arg1, arg2) }

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

// PromptLabService
export function RunPromptLab(arg1: number, arg2: number, arg3: boolean): Promise<PromptLabRunDTO> { return call('PromptLabService', 'RunPromptLab', arg1, arg2, arg3) }
export function ApproveProposal(arg1: string): Promise<Task> { return call('PromptLabService', 'ApproveProposal', arg1) }
export function RejectProposal(arg1: string, arg2: string): Promise<Task> { return call('PromptLabService', 'RejectProposal', arg1, arg2) }

// ProjectService
export function AdoptProject(arg1: string, arg2: string, arg3: string): Promise<Project> { return call('ProjectService', 'AdoptProject', arg1, arg2, arg3) }
export function CreateProject(arg1: string, arg2: string): Promise<Project> { return call('ProjectService', 'CreateProject', arg1, arg2) }
export function DeleteProject(arg1: string): Promise<void> { return call('ProjectService', 'DeleteProject', arg1) }
export function GetProject(arg1: string): Promise<Project> { return call('ProjectService', 'GetProject', arg1) }
export function ListProjects(): Promise<Array<Project>> { return call('ProjectService', 'ListProjects') }
export function ListWorktrees(arg1: string): Promise<Array<Worktree>> { return call('ProjectService', 'ListWorktrees', arg1) }
export function OpenInEditor(arg1: string): Promise<void> { return call('ProjectService', 'OpenInEditor', arg1) }
export function OpenInTerminal(arg1: string): Promise<void> { return call('ProjectService', 'OpenInTerminal', arg1) }
export function SetProjectWorktreeBaseRef(arg1: string, arg2: string): Promise<Project> { return call('ProjectService', 'SetProjectWorktreeBaseRef', arg1, arg2) }
export function SetProjectSetupCommands(arg1: string, arg2: string[]): Promise<Project> { return call('ProjectService', 'SetProjectSetupCommands', arg1, arg2) }
export function SetProjectSandboxConfig(arg1: string, arg2: unknown): Promise<Project> { return call('ProjectService', 'SetProjectSandboxConfig', arg1, arg2) }
export function GetProjectRawType(arg1: string): Promise<string> { return call('ProjectService', 'GetProjectRawType', arg1) }
export function CreateProjectAndClone(arg1: string, arg2: string): Promise<Project> { return call('ProjectService', 'CreateProjectAndClone', arg1, arg2) }
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
export function ScanEvaluation(): Promise<EvaluationReport> { return call('StatsService', 'ScanEvaluation') }

// LearningService
export function ListDigests(): Promise<Array<Digest>> { return call('LearningService', 'ListDigests') }
export function GetLatestDigest(): Promise<[Digest, boolean]> { return call('LearningService', 'GetLatestDigest') }

// TaskService
export function BlessTampering(arg1: string): Promise<Task> { return call('TaskService', 'BlessTampering', arg1) }
export function CreateTask(arg1: string, arg2: string, arg3: string): Promise<Task> { return call('TaskService', 'CreateTask', arg1, arg2, arg3) }
export function DeleteTask(arg1: string): Promise<void> { return call('TaskService', 'DeleteTask', arg1) }
export function CreateTaskFull(arg1: string, arg2: string, arg3: string, arg4: string, arg5: Update): Promise<Task> { return call('TaskService', 'CreateTaskFull', arg1, arg2, arg3, arg4, arg5) }
export function UpdateTaskFields(arg1: string, arg2: Update): Promise<Task> { return call('TaskService', 'UpdateTaskFields', arg1, arg2) }
export function ApplyTransition(arg1: TransitionIntent): Promise<TransitionResult> { return call('TaskService', 'ApplyTransition', arg1) }
export function TouchTask(arg1: string): Promise<Task> { return call('TaskService', 'TouchTask', arg1) }
export function ListTrash(): Promise<Array<TrashEntry>> { return call('TaskService', 'ListTrash') }
export function RestoreFromTrash(arg1: string): Promise<Task> { return call('TaskService', 'RestoreFromTrash', arg1) }
export function DeleteTrashedGeneration(arg1: string): Promise<boolean> { return call('TaskService', 'DeleteTrashedGeneration', arg1) }
export function PruneAllTrash(): Promise<TrashPruneReportDTO> { return call('TaskService', 'PruneAllTrash') }
export function ExpandUmbrella(arg1: string, arg2: string): Promise<UmbrellaExpandDTO> { return call('TaskService', 'ExpandUmbrella', arg1, arg2) }
export function ClassifyTask(arg1: string, arg2: string): Promise<TriageResultDTO> { return call('TaskService', 'ClassifyTask', arg1, arg2) }
export function AppendTaskProgress(arg1: string, arg2: string, arg3: string, arg4: string): Promise<ProgressEntry> { return call('TaskService', 'AppendTaskProgress', arg1, arg2, arg3, arg4) }
export function ListTaskArtifactMetas(arg1: string): Promise<Array<Meta>> { return call('TaskService', 'ListTaskArtifactMetas', arg1) }
export function ReadTaskArtifact(arg1: string, arg2: string): Promise<string> { return call('TaskService', 'ReadTaskArtifact', arg1, arg2) }
export function ReindexTaskArtifacts(arg1: string): Promise<void> { return call('TaskService', 'ReindexTaskArtifacts', arg1) }
export function ListTaskSnapshotHistory(arg1: number): Promise<Array<TaskHistoryEntryDTO>> { return call('TaskService', 'ListTaskSnapshotHistory', arg1) }
export function MapDuplicateIncidents(arg1: string, arg2: number[], arg3: string): Promise<MapDuplicateIncidentsDTO> { return call('TaskService', 'MapDuplicateIncidents', arg1, arg2, arg3) }

// AuditService
export function QueryAuditEvents(arg1: AuditQuery): Promise<Array<AuditEvent>> { return call('AuditService', 'QueryAuditEvents', arg1) }

// SelfMonitorService
export function GetSelfMonitorReport(): Promise<SelfMonitorReport> { return call('SelfMonitorService', 'GetSelfMonitorReport') }
export function InvestigateSelfMonitor(): Promise<SelfMonitorReport> { return call('SelfMonitorService', 'InvestigateSelfMonitor') }
export function ListSelfMonitorLedger(arg1: string, arg2: number): Promise<Array<LedgerEntry>> { return call('SelfMonitorService', 'ListSelfMonitorLedger', arg1, arg2) }
export function RunHarnessEvolution(arg1: number, arg2: number, arg3: string, arg4: boolean): Promise<HarnessEvolutionRunDTO> { return call('SelfMonitorService', 'RunHarnessEvolution', arg1, arg2, arg3, arg4) }
export function ScanMonitor(): Promise<MonitorReport> { return call('TaskService', 'ScanMonitor') }
export function DeleteAttachment(arg1: string, arg2: string): Promise<void> { return call('TaskService', 'DeleteAttachment', arg1, arg2) }
export function DispatchFromHumanRequired(arg1: string, arg2: string, arg3: string): Promise<Task> { return call('TaskService', 'DispatchFromHumanRequired', arg1, arg2, arg3) }
export function GetAttachmentURL(arg1: string, arg2: string): Promise<string> { return call('TaskService', 'GetAttachmentURL', arg1, arg2) }
export function ListTaskArtifacts(arg1: string): Promise<Array<TaskArtifactDTO>> { return call('TaskService', 'ListTaskArtifacts', arg1) }
export function ListAttachments(arg1: string): Promise<Array<any>> { return call('TaskService', 'ListAttachments', arg1) }
export function GetTaskSetupLog(arg1: string): Promise<TaskSetupLogDTO> { return call('TaskService', 'GetTaskSetupLog', arg1) }
export function ListTaskAuditEvents(arg1: string, arg2: number): Promise<Array<TaskAuditEventDTO>> { return call('TaskService', 'ListTaskAuditEvents', arg1, arg2) }
export function GetTamperReport(arg1: string): Promise<TamperReportDTO> { return call('TaskService', 'GetTamperReport', arg1) }
export function GetTask(arg1: string): Promise<Task> { return call('TaskService', 'GetTask', arg1) }
export function ListTasks(): Promise<Array<Task>> { return call('TaskService', 'ListTasks') }
export function ListTasksForNode(arg1: string): Promise<Array<Task>> { return call('TaskService', 'ListTasksForNode', arg1) }
export function ListTaskProgress(arg1: string): Promise<Array<ProgressEntry>> { return call('TaskService', 'ListTaskProgress', arg1) }
export function UploadAttachment(arg1: string, arg2: string, arg3: Array<number>): Promise<any> { return call('TaskService', 'UploadAttachment', arg1, arg2, arg3) }
export function UpdateTask(arg1: string, arg2: Record<string, unknown>): Promise<Task> { return call('TaskService', 'UpdateTask', arg1, arg2) }
export function AssignTask(arg1: Task): Promise<void> { return call('TaskService', 'AssignTask', arg1) }
export function RecoverLostAgent(arg1: string): Promise<void> { return call('TaskService', 'RecoverLostAgent', arg1) }

// WorkflowService
export function DeleteWorkflow(arg1: string): Promise<void> { return call('WorkflowService', 'DeleteWorkflow', arg1) }
export function GetWorkflow(arg1: string): Promise<Definition> { return call('WorkflowService', 'GetWorkflow', arg1) }
export function HandleHumanAction(arg1: string, arg2: string, arg3: Record<string, string>): Promise<void> { return call('WorkflowService', 'HandleHumanAction', arg1, arg2, arg3) }
export function ListWorkflows(): Promise<Array<Definition>> { return call('WorkflowService', 'ListWorkflows') }
export function ResetBuiltin(arg1: string): Promise<void> { return call('WorkflowService', 'ResetBuiltin', arg1) }
export function SaveWorkflow(arg1: Definition): Promise<void> { return call('WorkflowService', 'SaveWorkflow', arg1) }
export function StartWorkflow(arg1: string, arg2: string): Promise<void> { return call('WorkflowService', 'StartWorkflow', arg1, arg2) }

// ClusterService
export function GetNodes(): Promise<Array<ClusterNodeDTO>> { return call('ClusterService', 'GetNodes') }
export function GetAgentOnNode(arg1: string, arg2: string): Promise<Agent> { return call('ClusterService', 'GetAgentOnNode', arg1, arg2) }
export function StopAgentOnNode(arg1: string, arg2: string): Promise<void> { return call('ClusterService', 'StopAgentOnNode', arg1, arg2) }
export function SendMessageToNode(arg1: string, arg2: string, arg3: string): Promise<void> { return call('ClusterService', 'SendMessageToNode', arg1, arg2, arg3) }
export function RespondApprovalOnNode(arg1: string, arg2: string, arg3: boolean): Promise<void> { return call('ClusterService', 'RespondApprovalOnNode', arg1, arg2, arg3) }
export function ApprovePlanOnNode(arg1: string, arg2: string): Promise<void> { return call('ClusterService', 'ApprovePlanOnNode', arg1, arg2) }
export function RejectPlanOnNode(arg1: string, arg2: string, arg3: string): Promise<void> { return call('ClusterService', 'RejectPlanOnNode', arg1, arg2, arg3) }
export function ListNodeAgents(): Promise<Array<Agent>> { return call('ClusterService', 'ListNodeAgents') }
export function GetAgentOutputOnNode(arg1: string, arg2: string): Promise<Array<StreamEvent>> { return call('ClusterService', 'GetAgentOutputOnNode', arg1, arg2) }
export function GetConvoOutputOnNode(arg1: string, arg2: string): Promise<Array<ConvoEvent>> { return call('ClusterService', 'GetConvoOutputOnNode', arg1, arg2) }
export function ReassignTask(arg1: string, arg2: string): Promise<void> { return call('ClusterService', 'ReassignTask', arg1, arg2) }

// Shared EventSource for the multiplexed /events SSE stream.
// All EventsOn subscriptions funnel through a single connection.
// EventSource cannot set an Authorization header, so the token travels as a
// query param instead — AuthMiddleware accepts either form, but only for SSE
// paths (see isSSEPath in internal/httpserve).
function eventsURL(): string {
  // Strip /api suffix to get server root, then append /events.
  const url = apiBase().replace(/\/api$/, '') + '/events'
  const token = getApiToken()
  return token ? `${url}?token=${encodeURIComponent(token)}` : url
}

// Connection state of the live event stream. The stream is the first thing to
// notice a server that went away, so the UI reads its health from here rather
// than waiting for the next user action to fail.
export type ConnectionState = 'connecting' | 'open' | 'lost'

type ConnectionListener = (state: ConnectionState, reconnected: boolean) => void

let _connectionState: ConnectionState = 'connecting'
let _wasOpen = false
const _connectionListeners = new Set<ConnectionListener>()

export function getConnectionState(): ConnectionState { return _connectionState }

// OnConnectionChange reports every transition. `reconnected` is true on the
// open that follows a loss, which is the signal for stores to refetch the
// state they missed while the stream was down.
export function OnConnectionChange(listener: ConnectionListener): () => void {
  _connectionListeners.add(listener)
  return () => { _connectionListeners.delete(listener) }
}

function setConnectionState(state: ConnectionState): void {
  // A drop after a successful open is a loss even while EventSource is already
  // retrying. Waiting for readyState CLOSED would never fire: the spec puts the
  // stream back to CONNECTING before the error event, so an ordinary server
  // restart reads as open -> connecting -> open and the refetch never runs.
  const reconnected = state === 'open' && _connectionState !== 'open' && _wasOpen
  if (state === 'open') _wasOpen = true
  if (state === _connectionState && !reconnected) return
  _connectionState = state
  for (const listener of _connectionListeners) listener(state, reconnected)
}

// Retry delay after a fatal stream close. EventSource retries a transport
// error itself but gives up for good on an HTTP error, so that case is
// rebuilt here instead.
const STREAM_REBUILD_MS = 5_000

let _sharedES: EventSource | null = null
let _rebuildTimer: ReturnType<typeof setTimeout> | null = null
// Every subscription, kept so a rebuilt stream carries them over. A caller
// holds its unsubscribe for the life of the app and never re-subscribes.
const _subscriptions = new Map<string, Set<(e: MessageEvent) => void>>()

function subscriptionCount(): number {
  let total = 0
  for (const handlers of _subscriptions.values()) total += handlers.size
  return total
}

function openStream(): EventSource | null {
  if (_sharedES) return _sharedES
  if (!getApiToken() && !promptForApiToken()) return null
  setConnectionState(_wasOpen ? 'lost' : 'connecting')
  const es = new EventSource(eventsURL())
  es.onopen = () => setConnectionState('open')
  es.onerror = () => {
    setConnectionState(_wasOpen ? 'lost' : 'connecting')
    if (es.readyState === EventSource.CLOSED) scheduleRebuild(es)
  }
  for (const [eventName, handlers] of _subscriptions) {
    for (const handler of handlers) es.addEventListener(eventName, handler)
  }
  _sharedES = es
  return es
}

// scheduleRebuild replaces a stream EventSource has abandoned. It stops
// retrying after an HTTP error, so without this the UI stays dark until a
// manual reload even once the server is back.
function scheduleRebuild(dead: EventSource): void {
  if (_sharedES !== dead || _rebuildTimer !== null) return
  dead.close()
  _sharedES = null
  _rebuildTimer = setTimeout(() => {
    _rebuildTimer = null
    if (subscriptionCount() > 0) openStream()
  }, STREAM_REBUILD_MS)
}

function closeStream(): void {
  if (_rebuildTimer !== null) {
    clearTimeout(_rebuildTimer)
    _rebuildTimer = null
  }
  _sharedES?.close()
  _sharedES = null
  _wasOpen = false
  setConnectionState('connecting')
}

// Runtime: EventsOn via multiplexed SSE stream (GET /events).
// All subscriptions share a single EventSource connection.
// The server uses SSE named-event format so each listener only fires for its event.
export function EventsOn(eventName: string, callback: (...data: any[]) => void): () => void {
  const handler = (e: MessageEvent) => {
    try { callback(JSON.parse(e.data as string)) } catch { callback(e.data) }
  }
  let handlers = _subscriptions.get(eventName)
  if (!handlers) {
    handlers = new Set()
    _subscriptions.set(eventName, handlers)
  }
  handlers.add(handler)

  const es = openStream()
  if (!es) {
    handlers.delete(handler)
    throw new Error('sybra server auth token required for live updates')
  }
  es.addEventListener(eventName, handler)

  return () => {
    _sharedES?.removeEventListener(eventName, handler)
    handlers.delete(handler)
    if (handlers.size === 0) _subscriptions.delete(eventName)
    if (subscriptionCount() === 0) closeStream()
  }
}

// resetEventStreamForTest drops the module-level stream so one test's
// EventSource never leaks into the next.
export function resetEventStreamForTest(): void {
  _subscriptions.clear()
  closeStream()
}

// BrowserOpenURL sends the URL to the browser on the host serving this board.
//
// window.open alone is not enough: the desktop window's webview implements no
// window-opening delegate, so it is a silent no-op there and the link does
// nothing at all. A board on another machine refuses the call, and a real
// browser then handles the tab itself.
export function BrowserOpenURL(url: string): void {
  void OpenExternal(url).catch(() => { window.open(url, '_blank') })
}

// Open hands the URL to an in-app window on the host serving this board.
export function Open(arg1: string): Promise<void> { return call('BrowserService', 'Open', arg1) }

// OpenExternal hands the URL to that host's default browser.
export function OpenExternal(arg1: string): Promise<void> { return call('BrowserService', 'OpenExternal', arg1) }

// OpenInAppBrowser keeps the desktop's in-app window and degrades to the host
// browser, then to a tab, anywhere that window does not exist.
export function OpenInAppBrowser(url: string): void {
  void Open(url).catch(() => BrowserOpenURL(url))
}
