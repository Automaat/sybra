import { SvelteMap } from 'svelte/reactivity'
import {
  StopAgent,
  ListAgents,
  GetAgentOutput,
  DiscoverAgents,
  StartAgent,
  StartChat,
  StopChat,
} from '$lib/api'
import { Agent, State, StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
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

class AgentStore extends EntityStore<Agent> {
  outputs = new SvelteMap<string, TimestampedStreamEvent[]>()
  stepTexts = new SvelteMap<string, string>()

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
    super(
      async () => {
        await DiscoverAgents()
        return ListAgents()
      },
      (a, b) => {
        const ta = a.startedAt ? new Date(a.startedAt).getTime() : 0
        const tb = b.startedAt ? new Date(b.startedAt).getTime() : 0
        return tb - ta
      },
    )
  }

  get agents() {
    return this.items
  }
  set agents(v: Map<string, Agent>) {
    this.items = v
  }

  byTask(taskID: string): Agent | undefined {
    return this.list.find((a) => a.taskId === taskID)
  }

  byState(state: string): Agent[] {
    if (state === 'all') return this.list
    return this.list.filter((a) => a.state === state)
  }

  async start(taskID: string, mode: string, prompt: string, includeTaskDescription: boolean): Promise<Agent> {
    const result = await StartAgent(taskID, mode, prompt, includeTaskDescription)
    this.set(result.id, result)
    this.outputs.set(result.id, [])
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
      a.state = State.StateStopped
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
    this.set(agentID, data)
  }
}

export const agentStore = new AgentStore()
