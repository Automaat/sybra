import { GetStats } from '$lib/api'
import type { StatsResponse } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'

class StatsStore {
  data = $state<StatsResponse | null>(null)
  loading = $state(false)
  error = $state('')

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      this.data = await GetStats()
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }
}

export const statsStore = new StatsStore()
