import { GetEvaluationReport, EventsOn } from '$lib/api'
import { EvaluationReport } from '../lib/events.js'
import type { Report } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

class EvaluationStore {
  data = $state<Report | null>(null)
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      this.data = await GetEvaluationReport()
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  listen(): void {
    this.stopListening()
    this.cancelListener = EventsOn(EvaluationReport, (report: Report) => {
      this.data = report
    })
  }

  stopListening(): void {
    if (this.cancelListener) {
      this.cancelListener()
      this.cancelListener = null
    }
  }
}

export const evaluationStore = new EvaluationStore()
