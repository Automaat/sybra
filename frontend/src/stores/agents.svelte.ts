import { StartAgent, StopAgent, ListAgents, GetAgentOutput, DiscoverAgents } from '../../wailsjs/go/main/App.js'
import { agent } from '../../wailsjs/go/models.js'

class AgentStore {
  agents = $state<Map<string, agent.Agent>>(new Map())
  outputs = $state<Map<string, agent.StreamEvent[]>>(new Map())
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null

  get list(): agent.Agent[] {
    return [...this.agents.values()].sort((a, b) => {
      const ta = a.startedAt ? new Date(a.startedAt).getTime() : 0
      const tb = b.startedAt ? new Date(b.startedAt).getTime() : 0
      return tb - ta
    })
  }

  byTask(taskID: string): agent.Agent | undefined {
    return this.list.find((a) => a.taskId === taskID)
  }

  byState(state: string): agent.Agent[] {
    if (state === 'all') return this.list
    return this.list.filter((a) => a.state === state)
  }

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      await DiscoverAgents()
      const result = await ListAgents()
      const map = new Map<string, agent.Agent>()
      for (const a of result ?? []) {
        map.set(a.id, a)
      }
      this.agents = map
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  async start(taskID: string, mode: string, prompt: string): Promise<agent.Agent> {
    const result = await StartAgent(taskID, mode, prompt)
    this.agents.set(result.id, result)
    this.outputs.set(result.id, [])
    return result
  }

  async stop(agentID: string): Promise<void> {
    await StopAgent(agentID)
    const a = this.agents.get(agentID)
    if (a) {
      a.state = 'stopped'
      this.agents.set(agentID, a)
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
    this.agents.set(agentID, data)
  }

  startPolling(): void {
    this.stopPolling()
    this.pollTimer = setInterval(() => this.load(), 5000)
  }

  stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer)
      this.pollTimer = null
    }
  }
}

export const agentStore = new AgentStore()
