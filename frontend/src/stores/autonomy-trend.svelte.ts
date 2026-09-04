import { GetAutonomyTrend } from '$lib/api'
import type { AutonomyTrend } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

// All-time / last-week / last-month autonomy plus a weekly trend. Pull-only:
// there's no push event, so the Evaluation page loads it alongside the
// scorecard and on Refresh.
class AutonomyTrendStore {
  data = $state<AutonomyTrend | null>(null)
  loading = $state(false)
  error = $state('')

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      this.data = await GetAutonomyTrend()
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }
}

export const autonomyTrendStore = new AutonomyTrendStore()
