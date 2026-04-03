import { FetchReviews } from '../../wailsjs/go/main/App.js'
import { EventsOn } from '../../wailsjs/runtime/runtime.js'
import type { github } from '../../wailsjs/go/models.js'

class ReviewStore {
  createdByMe = $state<github.PullRequest[]>([])
  reviewRequested = $state<github.PullRequest[]>([])
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  get totalCount(): number {
    return this.createdByMe.length + this.reviewRequested.length
  }

  get allPRs(): github.PullRequest[] {
    const seen = new Set<string>()
    const result: github.PullRequest[] = []
    for (const pr of [...this.createdByMe, ...this.reviewRequested]) {
      const key = `${pr.repository}#${pr.number}`
      if (!seen.has(key)) {
        seen.add(key)
        result.push(pr)
      }
    }
    return result
  }

  byRepo(repo: string): github.PullRequest[] {
    return this.allPRs.filter((pr) => pr.repository === repo)
  }

  byTask(task: { projectId?: string; prNumber?: number; branch?: string }): github.PullRequest[] {
    if (!task.projectId) return []
    const repoPRs = this.byRepo(task.projectId)
    if (task.prNumber) {
      const exact = repoPRs.filter((pr) => pr.number === task.prNumber)
      if (exact.length > 0) return exact
    }
    if (task.branch) {
      const byBranch = repoPRs.filter((pr) => pr.headRefName === task.branch)
      if (byBranch.length > 0) return byBranch
    }
    return []
  }

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      const result = await FetchReviews()
      this.createdByMe = result.createdByMe ?? []
      this.reviewRequested = result.reviewRequested ?? []
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  listen(): void {
    this.stopListening()
    this.cancelListener = EventsOn('reviews:updated', (summary: any) => {
      this.createdByMe = summary.createdByMe ?? []
      this.reviewRequested = summary.reviewRequested ?? []
    })
  }

  stopListening(): void {
    if (this.cancelListener) {
      this.cancelListener()
      this.cancelListener = null
    }
  }

  // Keep for manual refresh from UI
  startPolling(): void {}
  stopPolling(): void {}
}

export const reviewStore = new ReviewStore()
if (typeof window !== 'undefined' && (window as any).runtime) {
  reviewStore.listen()
}
