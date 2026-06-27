import { GetLifecyclePhases } from '$lib/api'
import type { PhaseReport } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

// Per-phase lifecycle-duration breakdown for landed tasks. Pull-only: there's no
// push event, so the Evaluation page loads it alongside the scorecard and on
// Refresh.
class LifecycleStore {
  data = $state<PhaseReport | null>(null)
  loading = $state(false)
  error = $state('')

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      this.data = await GetLifecyclePhases()
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }
}

export const lifecycleStore = new LifecycleStore()
