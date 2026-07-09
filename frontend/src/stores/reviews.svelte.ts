import { FetchReviews, EventsOn } from '$lib/api'
import { ReviewsUpdated } from '../lib/events.js'
import type { PullRequest, ReviewSummary } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

class ReviewStore {
  createdByMe = $state<PullRequest[]>([])
  reviewRequested = $state<PullRequest[]>([])
  reviewedByMe = $state<PullRequest[]>([])
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  get totalCount(): number {
    return this.createdByMe.length + this.reviewRequested.length
  }

  get allPRs(): PullRequest[] {
    const seen = new Set<string>()
    const result: PullRequest[] = []
    for (const pr of [...this.createdByMe, ...this.reviewRequested, ...this.reviewedByMe]) {
      const key = `${pr.repository}#${pr.number}`
      if (!seen.has(key)) {
        seen.add(key)
        result.push(pr)
      }
    }
    return result
  }

  byRepo(repo: string): PullRequest[] {
    return this.allPRs.filter((pr) => pr.repository === repo)
  }

  byTask(task: { projectId?: string; prNumber?: number; branch?: string }): PullRequest[] {
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
      this.reviewedByMe = result.reviewedByMe ?? []
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  listen(): void {
    this.stopListening()
    this.cancelListener = EventsOn(ReviewsUpdated, (summary: ReviewSummary) => {
      this.createdByMe = summary.createdByMe ?? []
      this.reviewRequested = summary.reviewRequested ?? []
      this.reviewedByMe = summary.reviewedByMe ?? []
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
if (typeof window !== 'undefined') {
  reviewStore.listen()
}
