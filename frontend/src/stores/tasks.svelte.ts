import {
  ListTasks,
  GetTask,
  CreateTask,
  UpdateTask,
  DeleteTask,
  ApprovePlan,
  RejectPlan,
} from '../../wailsjs/go/main/App.js'
import { task } from '../../wailsjs/go/models.js'
import { EntityStore } from './entity-store.svelte.js'

class TaskStore extends EntityStore<task.Task> {
  constructor() {
    super(
      () => ListTasks(),
      (a, b) => {
        const ta = a.updatedAt ? new Date(a.updatedAt).getTime() : 0
        const tb = b.updatedAt ? new Date(b.updatedAt).getTime() : 0
        return tb - ta
      },
    )
  }

  get tasks() {
    return this.items
  }
  set tasks(v: Map<string, task.Task>) {
    this.items = v
  }

  byStatus(status: string): task.Task[] {
    if (status === 'all') return this.list
    return this.list.filter((t) => t.status === status)
  }

  async get(id: string): Promise<task.Task> {
    const result = await GetTask(id)
    this.set(result.id, result)
    return result
  }

  async create(title: string, body: string, mode: string): Promise<task.Task> {
    const result = await CreateTask(title, body, mode)
    this.set(result.id, result)
    return result
  }

  async update(id: string, updates: Record<string, any>): Promise<task.Task> {
    const result = await UpdateTask(id, updates)
    this.set(result.id, result)
    return result
  }

  async remove(id: string): Promise<void> {
    await DeleteTask(id)
    this.delete(id)
  }

  async approvePlan(id: string): Promise<task.Task> {
    const result = await ApprovePlan(id)
    this.set(result.id, result)
    return result
  }

  async rejectPlan(id: string, feedback: string): Promise<task.Task> {
    const result = await RejectPlan(id, feedback)
    this.set(result.id, result)
    return result
  }
}

export const taskStore = new TaskStore()
