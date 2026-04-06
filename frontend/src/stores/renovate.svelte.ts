import { FetchRenovatePRs } from '../../wailsjs/go/main/App.js'
import { EventsOn } from '../../wailsjs/runtime/runtime.js'
import { RenovateUpdated } from '../lib/events.js'
import type { github } from '../../wailsjs/go/models.js'

class RenovateStore {
  prs = $state<github.RenovatePR[]>([])
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  get count(): number {
    return this.prs.length
  }

  get eligible(): github.RenovatePR[] {
    return this.prs.filter(
      (pr) =>
        !pr.isDraft &&
        pr.mergeable === 'MERGEABLE' &&
        (pr.ciStatus === 'SUCCESS' || pr.ciStatus === ''),
    )
  }

  get failing(): github.RenovatePR[] {
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
    this.cancelListener = EventsOn(RenovateUpdated, (prs: any) => {
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
if (typeof window !== 'undefined' && (window as any).runtime) {
  renovateStore.listen()
}
