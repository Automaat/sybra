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
import { agent } from '../../wailsjs/go/models.js'
import { EntityStore } from './entity-store.svelte.js'
import { extractStepText } from '$lib/step-text.js'
import type { TimestampedStreamEvent } from '$lib/timeline.js'

class AgentStore extends EntityStore<agent.Agent> {
  outputs = new SvelteMap<string, TimestampedStreamEvent[]>()
  stepTexts = new SvelteMap<string, string>()

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
  set agents(v: Map<string, agent.Agent>) {
    this.items = v
  }

  byTask(taskID: string): agent.Agent | undefined {
    return this.list.find((a) => a.taskId === taskID)
  }

  byState(state: string): agent.Agent[] {
    if (state === 'all') return this.list
    return this.list.filter((a) => a.state === state)
  }

  async start(taskID: string, mode: string, prompt: string): Promise<agent.Agent> {
    const result = await StartAgent(taskID, mode, prompt)
    this.set(result.id, result)
    this.outputs.set(result.id, [])
    return result
  }

  async startChat(projectID: string, provider: string, prompt: string): Promise<agent.Agent> {
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

  appendEvent(agentID: string, event: agent.StreamEvent): void {
    const tse: TimestampedStreamEvent = { event, receivedAt: new Date() }
    const existing = this.outputs.get(agentID) ?? []
    this.outputs.set(agentID, [...existing, tse])
    const text = extractStepText(event)
    if (text) this.stepTexts.set(agentID, text)
  }

  setStepText(agentID: string, text: string): void {
    this.stepTexts.set(agentID, text)
  }

  updateAgent(agentID: string, data: agent.Agent): void {
    this.set(agentID, data)
  }
}

export const agentStore = new AgentStore()
