import { GetStats } from '../../wailsjs/go/main/App.js'
import type { stats } from '../../wailsjs/go/models.js'

class StatsStore {
  data = $state<stats.StatsResponse | null>(null)
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
