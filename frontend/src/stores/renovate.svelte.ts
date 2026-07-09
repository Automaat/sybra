import { FetchRenovatePRs, EventsOn } from '$lib/api'
import { RenovateUpdated } from '../lib/events.js'
import type { RenovatePR } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

class RenovateStore {
  prs = $state<RenovatePR[]>([])
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  get count(): number {
    return this.prs.length
  }

  get eligible(): RenovatePR[] {
    return this.prs.filter(
      (pr) =>
        !pr.isDraft &&
        pr.mergeable === 'MERGEABLE' &&
        (pr.ciStatus === 'SUCCESS' || pr.ciStatus === '' || pr.waitingForStability),
    )
  }

  get failing(): RenovatePR[] {
    return this.prs.filter((pr) => pr.ciStatus === 'FAILURE')
  }

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      const result = await FetchRenovatePRs()
      this.prs = result ?? []
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  listen(): void {
    this.stopListening()
    this.cancelListener = EventsOn(RenovateUpdated, (prs: RenovatePR[]) => {
      this.prs = prs ?? []
    })
  }

  stopListening(): void {
    if (this.cancelListener) {
      this.cancelListener()
      this.cancelListener = null
    }
  }

  startPolling(): void {}
  stopPolling(): void {}
}

export const renovateStore = new RenovateStore()
if (typeof window !== 'undefined') {
  renovateStore.listen()
}
