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
    return repoPRs
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
