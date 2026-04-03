import { FetchReviews } from '../../wailsjs/go/main/App.js'
import type { github } from '../../wailsjs/go/models.js'

class ReviewStore {
  createdByMe = $state<github.PullRequest[]>([])
  reviewRequested = $state<github.PullRequest[]>([])
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null

  get totalCount(): number {
    return this.createdByMe.length + this.reviewRequested.length
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

  startPolling(): void {
    this.stopPolling()
    this.pollTimer = setInterval(() => this.load(), 60_000)
  }

  stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer)
      this.pollTimer = null
    }
  }
}

export const reviewStore = new ReviewStore()
