import {
  ListLoopAgents,
  GetLoopAgent,
  CreateLoopAgent,
  UpdateLoopAgent,
  DeleteLoopAgent,
  RunLoopAgentNow,
  ListLoopAgentRuns,
} from '$lib/api'
import { loopagent } from '../../wailsjs/go/models.js'
import type { main } from '../../wailsjs/go/models.js'
import { EntityStore } from './entity-store.svelte.js'

class LoopStore extends EntityStore<loopagent.LoopAgent> {
  constructor() {
    super(
      () => ListLoopAgents(),
      (a, b) => a.name.localeCompare(b.name),
    )
  }

  async get(id: string): Promise<loopagent.LoopAgent> {
    const result = await GetLoopAgent(id)
    this.set(result.id, result)
    return result
  }

  async create(la: Partial<loopagent.LoopAgent>): Promise<loopagent.LoopAgent> {
    const input = new loopagent.LoopAgent(la)
    const result = await CreateLoopAgent(input)
    this.set(result.id, result)
    return result
  }

  async update(la: loopagent.LoopAgent): Promise<loopagent.LoopAgent> {
    const result = await UpdateLoopAgent(la)
    this.set(result.id, result)
    return result
  }

  async remove(id: string): Promise<void> {
    await DeleteLoopAgent(id)
    this.delete(id)
  }

  async runNow(id: string): Promise<string> {
    const agentId = await RunLoopAgentNow(id)
    await this.get(id)
    return agentId
  }

  async runs(id: string, limit = 10): Promise<main.LoopAgentRun[]> {
    return (await ListLoopAgentRuns(id, limit)) ?? []
  }
}

export const loopStore = new LoopStore()
