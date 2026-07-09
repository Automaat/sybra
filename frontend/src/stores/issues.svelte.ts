import { FetchAssignedIssues, EventsOn } from '$lib/api'
import { IssuesUpdated } from '../lib/events.js'
import type { Issue } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

class IssueStore {
  issues = $state<Issue[]>([])
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  get count(): number {
    return this.issues.length
  }

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      const result = await FetchAssignedIssues()
      this.issues = result ?? []
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  listen(): void {
    this.stopListening()
    this.cancelListener = EventsOn(IssuesUpdated, (issues: Issue[]) => {
      this.issues = issues ?? []
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

export const issueStore = new IssueStore()
if (typeof window !== 'undefined') {
  // Desktop mode needs window.runtime (Wails IPC); web mode uses SSE directly.
  if (import.meta.env.VITE_MODE === 'web' || window.runtime) {
    // Guard against a synchronous throw (e.g. web-mode token prompt cancelled)
    // poisoning the lazy import chunk this module lives in.
    try {
      issueStore.listen()
    } catch (e) {
      console.warn('issueStore.listen() failed to attach:', e)
    }
  }
}
