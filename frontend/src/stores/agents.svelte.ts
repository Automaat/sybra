import {
  StartAgent,
  StopAgent,
  ListAgents,
  GetAgentOutput,
  DiscoverAgents,
} from '../../wailsjs/go/main/App.js'
import { agent } from '../../wailsjs/go/models.js'
import { EntityStore } from './entity-store.svelte.js'

class AgentStore extends EntityStore<agent.Agent> {
  outputs = $state<Map<string, agent.StreamEvent[]>>(new Map())

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

  async stop(agentID: string): Promise<void> {
    await StopAgent(agentID)
    const a = this.items.get(agentID)
    if (a) {
      a.state = 'stopped'
      this.set(agentID, a)
    }
  }

  async getOutput(agentID: string): Promise<agent.StreamEvent[]> {
    const events = await GetAgentOutput(agentID)
    this.outputs.set(agentID, events ?? [])
    return events ?? []
  }

  appendEvent(agentID: string, event: agent.StreamEvent): void {
    const existing = this.outputs.get(agentID) ?? []
    this.outputs.set(agentID, [...existing, event])
  }

  updateAgent(agentID: string, data: agent.Agent): void {
    this.set(agentID, data)
  }
}

export const agentStore = new AgentStore()
