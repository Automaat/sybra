import { SvelteMap } from 'svelte/reactivity'
import {
  StopAgent,
  ListAgents,
  GetAgentOutput,
  DiscoverAgents,
  StartAgent,
  StartChat,
  StopChat,
  AgentQueueSnapshot as FetchAgentQueueSnapshot,
} from '$lib/api'
import { StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import type { Agent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import type {
  AgentQueueSnapshot,
  AgentQueueSnapshotItem,
} from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
import { EntityStore } from './entity-store.svelte.js'
import { extractStepText } from '$lib/step-text.js'
import type { TimestampedStreamEvent } from '$lib/timeline.js'

// Per-task agent-status flags, precomputed once per agent-list change so a
// card reads O(1) instead of scanning the whole agent list 4× per render.
// Mirrors the running/role predicates TaskCard used to compute inline.
export type AgentTaskStatus = {
  triaging: boolean
  evaluating: boolean
  planning: boolean
  running: boolean
}

type QueueAwareAgent = Agent & {
  queuePosition?: number
  queueDepth?: number
  queuePriority?: string
  queueEffectivePriority?: string
  queueStatus?: string
  queueManual?: boolean
  queueRole?: string
  queueEnqueued?: string
}

function syntheticQueuedAgentID(taskID: string): string {
  return `queued-${taskID}`
}

function isSyntheticQueuedAgent(agent: Agent): boolean {
  return Boolean(
    agent.taskId &&
    agent.state === 'queued' &&
    agent.id === syntheticQueuedAgentID(agent.taskId),
  )
}

function applyQueueMetadata(agent: Agent, row?: AgentQueueSnapshotItem): QueueAwareAgent {
  const next = { ...agent } as QueueAwareAgent
  if (!row) {
    delete next.queuePosition
    delete next.queueDepth
    delete next.queuePriority
    delete next.queueEffectivePriority
    delete next.queueStatus
    delete next.queueManual
    delete next.queueRole
    delete next.queueEnqueued
    return next
  }
  next.queuePosition = row.position
  next.queueDepth = row.depth
  next.queuePriority = row.priority
  next.queueEffectivePriority = row.effectivePriority
  next.queueStatus = row.status
  next.queueManual = row.manual
  next.queueRole = row.role
  next.queueEnqueued = row.enqueued
  return next
}

function buildQueueMap(snapshot: AgentQueueSnapshot | null | undefined): Map<string, AgentQueueSnapshotItem> {
  const map = new Map<string, AgentQueueSnapshotItem>()
  for (const row of snapshot?.items ?? []) {
    map.set(row.taskId, row)
  }
  return map
}

function mergeAgentsByID(primary: Agent[], secondary: Agent[]): Agent[] {
  const merged = new Map<string, Agent>()
  for (const agent of primary) merged.set(agent.id, agent)
  for (const agent of secondary) merged.set(agent.id, agent)
  return [...merged.values()]
}

class AgentStore extends EntityStore<Agent> {
  outputs = new SvelteMap<string, TimestampedStreamEvent[]>()
  stepTexts = new SvelteMap<string, string>()
  queueByTask = $state<Map<string, AgentQueueSnapshotItem>>(new Map())

  // taskId → role flags for that task's running agents. Recomputed once when
  // the agent list changes; cards index into it by task id (see TaskCard).
  // Each running agent maps to exactly one flag, matching the original
  // mutually-exclusive prefix predicates (triage:/eval:/plan:, else generic).
  agentStatusByTask = $derived.by(() => {
    const map = new Map<string, AgentTaskStatus>()
    for (const a of this.list) {
      if (a.state !== 'running' || !a.taskId) continue
      let s = map.get(a.taskId)
      if (!s) {
        s = { triaging: false, evaluating: false, planning: false, running: false }
        map.set(a.taskId, s)
      }
      const name = a.name ?? ''
      if (name.startsWith('triage:')) s.triaging = true
      else if (name.startsWith('eval:')) s.evaluating = true
      else if (name.startsWith('plan:')) s.planning = true
      else s.running = true
    }
    return map
  })

  constructor() {
    let self: AgentStore
    super(
      async () => self.loadAgents(),
      (a, b) => {
        const ta = a.startedAt ? new Date(a.startedAt).getTime() : 0
        const tb = b.startedAt ? new Date(b.startedAt).getTime() : 0
        return tb - ta
      },
    )
    self = this
  }

  get agents() {
    return this.items
  }
  set agents(v: Map<string, Agent>) {
    this.items = v
  }

  byTask(taskID: string): Agent | undefined {
    const real = this.list.find((a) => a.taskId === taskID && !isSyntheticQueuedAgent(a))
    if (real) return real
    return this.list.find((a) => a.taskId === taskID)
  }

  byState(state: string): Agent[] {
    if (state === 'all') return this.list
    return this.list.filter((a) => a.state === state)
  }

  async start(taskID: string, mode: string, prompt: string, includeTaskDescription: boolean): Promise<Agent> {
    const result = await StartAgent(taskID, mode, prompt, includeTaskDescription)
    this.set(result.id, applyQueueMetadata(result, this.queueByTask.get(taskID)))
    this.outputs.set(result.id, [])
    if (result.state === 'queued') {
      await this.refreshQueueSnapshot()
    }
    return result
  }

  async startChat(projectID: string, provider: string, prompt: string): Promise<Agent> {
    const result = await StartChat(projectID, provider, prompt)
    this.set(result.id, result)
    this.outputs.set(result.id, [])
    return result
  }

  async stop(agentID: string): Promise<void> {
    await StopAgent(agentID)
    const a = this.items.get(agentID)
    if (a) {
      a.state = 'stopped'
      this.set(agentID, a)
    }
  }

  async stopChat(agentID: string): Promise<void> {
    await StopChat(agentID)
    this.items.delete(agentID)
    this.outputs.delete(agentID)
  }

  async getOutput(agentID: string): Promise<TimestampedStreamEvent[]> {
    const events = await GetAgentOutput(agentID)
    const list = events ?? []
    const now = new Date()
    const wrapped: TimestampedStreamEvent[] = list.map((e) => ({ event: e, receivedAt: now }))
    this.outputs.set(agentID, wrapped)
    for (let i = wrapped.length - 1; i >= 0; i--) {
      const text = extractStepText(wrapped[i].event)
      if (text) {
        this.stepTexts.set(agentID, text)
        break
      }
    }
    return wrapped
  }

  appendEvent(agentID: string, event: StreamEvent): void {
    const tse: TimestampedStreamEvent = { event, receivedAt: new Date() }
    const existing = this.outputs.get(agentID) ?? []
    this.outputs.set(agentID, [...existing, tse])
    const text = extractStepText(event)
    if (text) this.stepTexts.set(agentID, text)
  }

  setStepText(agentID: string, text: string): void {
    this.stepTexts.set(agentID, text)
  }

  updateAgent(agentID: string, data: Agent): void {
    this.set(agentID, applyQueueMetadata(data, data.taskId ? this.queueByTask.get(data.taskId) : undefined))
  }

  private async loadAgents(): Promise<Agent[]> {
    await DiscoverAgents()
    const listed = await ListAgents()
    try {
      const snapshot = await FetchAgentQueueSnapshot()
      return this.reconcileQueue(listed ?? [], snapshot, true)
    } catch {
      return this.reconcileQueue(listed ?? [], null, false)
    }
  }

  private async refreshQueueSnapshot(): Promise<void> {
    try {
      const snapshot = await FetchAgentQueueSnapshot()
      const realAgents = [...this.items.values()].filter((agent) => !isSyntheticQueuedAgent(agent))
      this.replaceAgents(this.reconcileQueue(realAgents, snapshot, true))
    } catch {
      // Queue metadata is supplemental: a successful StartAgent/ListAgents path
      // must stay usable even when the snapshot readout fails independently.
    }
  }

  private reconcileQueue(realAgents: Agent[], snapshot: AgentQueueSnapshot | null, snapshotOK: boolean): Agent[] {
    const queue = snapshotOK ? buildQueueMap(snapshot) : new Map(this.queueByTask)
    this.queueByTask = queue

    const previousSyntheticByTask = new Map<string, QueueAwareAgent>()
    for (const agent of this.items.values()) {
      if (agent.taskId && isSyntheticQueuedAgent(agent)) {
        previousSyntheticByTask.set(agent.taskId, agent as QueueAwareAgent)
      }
    }

    const withRealPrecedence = new Set<string>()
    const mergedReal = mergeAgentsByID(realAgents, []).map((agent) => {
      if (agent.taskId) withRealPrecedence.add(agent.taskId)
      return applyQueueMetadata(agent, agent.taskId ? queue.get(agent.taskId) : undefined)
    })

    const synthetic: Agent[] = []
    for (const [taskID, row] of queue.entries()) {
      if (withRealPrecedence.has(taskID)) continue
      const existing = previousSyntheticByTask.get(taskID)
      synthetic.push(applyQueueMetadata(existing ?? {
        id: syntheticQueuedAgentID(taskID),
        taskId: taskID,
        mode: row.mode || 'headless',
        state: 'queued',
        sessionId: '',
        costUsd: 0,
        startedAt: row.enqueued,
        lastEventAt: row.enqueued,
        external: false,
      } as Agent, row))
    }

    return [...mergedReal, ...synthetic]
  }

  private replaceAgents(agents: Agent[]): void {
    const map = new Map<string, Agent>()
    for (const agent of agents) {
      map.set(agent.id, agent)
    }
    this.agents = map
  }
}

export const agentStore = new AgentStore()
