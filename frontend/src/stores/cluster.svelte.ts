import { GetNodes } from '$lib/api'
import type { ClusterNodeDTO } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

class ClusterStore {
  nodes = $state<ClusterNodeDTO[]>([])
  loading = $state(false)
  error = $state('')

  get enabled(): boolean {
    return this.nodes.length > 0
  }

  get names(): string[] {
    return this.nodes.map((n) => n.name)
  }

  async load(): Promise<void> {
    this.loading = true
    try {
      this.nodes = await GetNodes()
      this.error = ''
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  statusOf(node: string | undefined): string {
    if (!node) return ''
    const found = this.nodes.find((n) => n.name === node)
    return found ? found.status : 'unknown'
  }

  private timer: ReturnType<typeof setInterval> | undefined

  startPolling(intervalMs = 15000): void {
    this.stopPolling()
    this.timer = setInterval(() => void this.load(), intervalMs)
  }

  stopPolling(): void {
    if (this.timer) {
      clearInterval(this.timer)
      this.timer = undefined
    }
  }
}

export const clusterStore = new ClusterStore()
