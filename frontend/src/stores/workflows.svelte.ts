import {
  ListWorkflows,
  GetWorkflow,
  SaveWorkflow,
  DeleteWorkflow,
  ResetBuiltin,
} from '$lib/api'
import { Definition } from '../../bindings/github.com/Automaat/sybra/internal/workflow/models.js'
import { EntityStore } from './entity-store.svelte.js'

class WorkflowStore extends EntityStore<Definition> {
  constructor() {
    super(
      () => ListWorkflows(),
      (a, b) => a.name.localeCompare(b.name),
    )
  }

  async get(id: string): Promise<Definition> {
    const result = await GetWorkflow(id)
    this.set(result.id, result)
    return result
  }

  async save(def: Definition): Promise<void> {
    await SaveWorkflow(def)
    this.set(def.id, def)
  }

  async remove(id: string): Promise<void> {
    await DeleteWorkflow(id)
    this.delete(id)
  }

  async resetBuiltin(id: string): Promise<void> {
    await ResetBuiltin(id)
    await this.load()
  }
}

export const workflowStore = new WorkflowStore()
