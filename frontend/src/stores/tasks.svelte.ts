import { ListTasks, GetTask, CreateTask, UpdateTask, DeleteTask, ApprovePlan, RejectPlan } from '../../wailsjs/go/main/App.js'
import { task } from '../../wailsjs/go/models.js'

class TaskStore {
  tasks = $state<Map<string, task.Task>>(new Map())
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null

  get list(): task.Task[] {
    return [...this.tasks.values()].sort((a, b) => {
      const ta = a.updatedAt ? new Date(a.updatedAt).getTime() : 0
      const tb = b.updatedAt ? new Date(b.updatedAt).getTime() : 0
      return tb - ta
    })
  }

  byStatus(status: string): task.Task[] {
    if (status === 'all') return this.list
    return this.list.filter((t) => t.status === status)
  }

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      const result = await ListTasks()
      const map = new Map<string, task.Task>()
      for (const t of result ?? []) {
        map.set(t.id, t)
      }
      this.tasks = map
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  async get(id: string): Promise<task.Task> {
    const result = await GetTask(id)
    this.tasks.set(result.id, result)
    return result
  }

  async create(title: string, body: string, mode: string): Promise<task.Task> {
    const result = await CreateTask(title, body, mode)
    this.tasks.set(result.id, result)
    return result
  }

  async update(id: string, updates: Record<string, any>): Promise<task.Task> {
    const result = await UpdateTask(id, updates)
    this.tasks.set(result.id, result)
    return result
  }

  async remove(id: string): Promise<void> {
    await DeleteTask(id)
    this.tasks.delete(id)
  }

  async approvePlan(id: string): Promise<task.Task> {
    const result = await ApprovePlan(id)
    this.tasks.set(result.id, result)
    return result
  }

  async rejectPlan(id: string, feedback: string): Promise<task.Task> {
    const result = await RejectPlan(id, feedback)
    this.tasks.set(result.id, result)
    return result
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

export const taskStore = new TaskStore()
