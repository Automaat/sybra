import { EventsOn, ListBackgroundOps } from '$lib/api'
import * as ev from '../lib/events.js'

export type BgOpStatus = 'running' | 'done' | 'failed'
export type BgOpType = 'clone' | 'worktree_prep'

export interface Operation {
  id: string
  type: BgOpType
  label: string
  status: BgOpStatus
  phase?: string
  projectId?: string
  taskId?: string
  startedAt: string
  completedAt?: string
  error?: string
}

// Auto-remove completed/failed ops after 5 minutes (mirrors backend TTL).
const COMPLETION_TTL_MS = 5 * 60 * 1000

class BgOpStore {
  ops = $state<Operation[]>([])

  get activeCount(): number {
    return this.ops.filter((o) => o.status === 'running').length
  }

  get hasActive(): boolean {
    return this.activeCount > 0
  }

  async load(): Promise<void> {
    const result = await ListBackgroundOps()
    this.ops = (result as Operation[] | null) ?? []
  }

  listen(): () => void {
    const unsubs = [
      EventsOn(ev.BgOpStarted, (op: Operation) => {
        this.ops = [op, ...this.ops.filter((o) => o.id !== op.id)]
      }),
      EventsOn(ev.BgOpProgress, (op: Operation) => {
        this.ops = this.ops.map((o) => (o.id === op.id ? op : o))
      }),
      EventsOn(ev.BgOpCompleted, (op: Operation) => {
        this.ops = this.ops.map((o) => (o.id === op.id ? op : o))
        setTimeout(() => {
          this.ops = this.ops.filter((o) => o.id !== op.id)
        }, COMPLETION_TTL_MS)
      }),
      EventsOn(ev.BgOpFailed, (op: Operation) => {
        this.ops = this.ops.map((o) => (o.id === op.id ? op : o))
        setTimeout(() => {
          this.ops = this.ops.filter((o) => o.id !== op.id)
        }, COMPLETION_TTL_MS)
      }),
    ]
    return () => unsubs.forEach((u) => u())
  }
}

export const bgopStore = new BgOpStore()
