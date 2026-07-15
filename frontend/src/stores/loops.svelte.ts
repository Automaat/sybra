import {
  ListLoopAgents,
  GetLoopAgent,
  CreateLoopAgent,
  UpdateLoopAgent,
  DeleteLoopAgent,
  RunLoopAgentNow,
  ListLoopAgentRuns,
} from '$lib/api'
import { LoopAgent } from '../../bindings/github.com/Automaat/sybra/internal/loopagent/models.js'
import type { LoopAgentRun } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
import { EntityStore } from './entity-store.svelte.js'

class LoopStore extends EntityStore<LoopAgent> {
  constructor() {
    super(
      () => ListLoopAgents(),
      (a, b) => a.name.localeCompare(b.name),
    )
  }

  async get(id: string): Promise<LoopAgent> {
    const result = await GetLoopAgent(id)
    this.set(result.id, result)
    return result
  }

  async create(la: Partial<LoopAgent>): Promise<LoopAgent> {
    const input = new LoopAgent(la)
    const result = await CreateLoopAgent(input)
    this.set(result.id, result)
    return result
  }

  async update(la: LoopAgent): Promise<LoopAgent> {
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

  async runs(id: string, limit = 10): Promise<LoopAgentRun[]> {
    return (await ListLoopAgentRuns(id, limit)) ?? []
  }
}

export const loopStore = new LoopStore()
