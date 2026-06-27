import {
  ListTasks,
  GetTask,
  CreateTask,
  UpdateTask,
  DeleteTask,
  ApprovePlan,
  RejectPlan,
  SendPlanMessage,
  HasLivePlanAgent,
} from '$lib/api'
import { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import { EntityStore } from './entity-store.svelte.js'

class TaskStore extends EntityStore<Task> {
  // Per-id operation counter guarding the async patchOne against its own
  // out-of-order completion. patchOne re-reads it after GetTask resolves and
  // drops the result if a newer op (a delete, or a later patch) has since run —
  // otherwise a slow in-flight fetch could resurrect a just-deleted task or
  // overwrite newer state, since the Map is last-write-wins. IDs are never
  // reused, so a monotonic per-id counter is sufficient.
  #opSeq = new Map<string, number>()

  #bumpSeq(id: string): number {
    const n = (this.#opSeq.get(id) ?? 0) + 1
    this.#opSeq.set(id, n)
    return n
  }

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
  set tasks(v: Map<string, Task>) {
    this.items = v
  }

  byStatus(status: string): Task[] {
    if (status === 'all') return this.list
    return this.list.filter((t) => t.status === status)
  }

  async get(id: string): Promise<Task> {
    const result = await GetTask(id)
    this.set(result.id, result)
    return result
  }

  // Fetch one task and upsert it into the reactive Map without rebuilding the
  // whole Map. Drives the live task:created/updated event handler so a single
  // changed file re-renders one card instead of forcing a full reload + total
  // re-render. Swallows errors: the id may have just vanished, or be derived
  // from a sidecar whose parent is gone — the trailing delete event or the
  // background poll reconciles.
  async patchOne(id: string): Promise<void> {
    const mine = this.#bumpSeq(id)
    try {
      const result = await GetTask(id)
      // Superseded by a later remove/patch while this fetch was in flight —
      // applying it now would resurrect a deleted task or clobber newer state.
      if (this.#opSeq.get(id) !== mine) return
      if (result?.id) this.set(result.id, result)
    } catch {
      // Task unreadable/removed — leave the Map untouched.
    }
  }

  // Drop one task from the reactive Map (pure local, no fetch). Drives the
  // task:deleted handler. Bumps the op counter so any in-flight patchOne for
  // this id is discarded when it resolves.
  removeOne(id: string): void {
    this.#bumpSeq(id)
    this.delete(id)
  }

  async create(title: string, body: string, mode: string): Promise<Task> {
    const result = await CreateTask(title, body, mode)
    this.set(result.id, result)
    return result
  }

  async update(id: string, updates: Record<string, any>): Promise<Task> {
    const result = await UpdateTask(id, updates)
    this.set(result.id, result)
    return result
  }

  async remove(id: string): Promise<void> {
    await DeleteTask(id)
    this.delete(id)
  }

  async approvePlan(id: string): Promise<Task> {
    const result = await ApprovePlan(id)
    this.set(result.id, result)
    return result
  }

  async rejectPlan(id: string, feedback: string): Promise<Task> {
    const result = await RejectPlan(id, feedback)
    this.set(result.id, result)
    return result
  }

  async sendPlanMessage(id: string, message: string): Promise<void> {
    await SendPlanMessage(id, message)
  }

  async hasLivePlanAgent(id: string): Promise<boolean> {
    return HasLivePlanAgent(id)
  }
}

export const taskStore = new TaskStore()
