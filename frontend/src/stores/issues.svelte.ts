import { FetchAssignedIssues } from '../../wailsjs/go/main/App.js'
import { EventsOn } from '../../wailsjs/runtime/runtime.js'
import { IssuesUpdated } from '../lib/events.js'
import type { github } from '../../wailsjs/go/models.js'

class IssueStore {
  issues = $state<github.Issue[]>([])
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
    this.cancelListener = EventsOn(IssuesUpdated, (issues: any) => {
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
if (typeof window !== 'undefined' && window.runtime) {
  issueStore.listen()
}
