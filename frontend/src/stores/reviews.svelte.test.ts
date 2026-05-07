import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ReviewsUpdated } from '../lib/events.js'
import { PullRequest } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

const mockFetchReviews = vi.fn()
let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('$lib/api', () => ({
  FetchReviews: (...args: unknown[]) => mockFetchReviews(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
}))

const { reviewStore } = await import('./reviews.svelte.js')

function makePR(overrides: Record<string, unknown> = {}): PullRequest {
  return PullRequest.createFrom({
    number: 1,
    title: 'Test PR',
    url: 'https://github.com/org/repo/pull/1',
    repository: 'org/repo',
    repoName: 'repo',
    author: 'user',
    isDraft: false,
    labels: [],
    headRefName: '',
    ciStatus: '',
    reviewDecision: '',
    mergeable: '',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  })
}

describe('ReviewStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    eventCallbacks = {}
    reviewStore.createdByMe = []
    reviewStore.reviewRequested = []
    reviewStore.error = ''
    reviewStore.loading = false
    reviewStore.stopListening()
  })

  describe('load', () => {
    it('fetches reviews from backend', async () => {
      const created = [makePR({ number: 1 })]
      const requested = [makePR({ number: 2 })]
      mockFetchReviews.mockResolvedValue({ createdByMe: created, reviewRequested: requested })

      await reviewStore.load()

      expect(mockFetchReviews).toHaveBeenCalled()
      expect(reviewStore.createdByMe).toHaveLength(1)
      expect(reviewStore.reviewRequested).toHaveLength(1)
    })

    it('handles null arrays', async () => {
      mockFetchReviews.mockResolvedValue({ createdByMe: null, reviewRequested: null })

      await reviewStore.load()

      expect(reviewStore.createdByMe).toHaveLength(0)
      expect(reviewStore.reviewRequested).toHaveLength(0)
      expect(reviewStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockFetchReviews.mockRejectedValue(new Error('gh not found'))

      await reviewStore.load()

      expect(reviewStore.error).toBe('Error: gh not found')
    })

    it('sets loading flag', async () => {
      mockFetchReviews.mockResolvedValue({ createdByMe: [], reviewRequested: [] })

      const promise = reviewStore.load()
      expect(reviewStore.loading).toBe(true)
      await promise
      expect(reviewStore.loading).toBe(false)
    })

    it('clears previous error on success', async () => {
      reviewStore.error = 'old error'
      mockFetchReviews.mockResolvedValue({ createdByMe: [], reviewRequested: [] })

      await reviewStore.load()

      expect(reviewStore.error).toBe('')
    })
  })

  describe('totalCount', () => {
    it('sums both lists', () => {
      reviewStore.createdByMe = [makePR({ number: 1 }), makePR({ number: 2 })]
      reviewStore.reviewRequested = [makePR({ number: 3 })]

      expect(reviewStore.totalCount).toBe(3)
    })

    it('returns 0 when empty', () => {
      expect(reviewStore.totalCount).toBe(0)
    })
  })

  describe('allPRs', () => {
    it('merges both lists deduplicating by repository+number', () => {
      const pr1 = makePR({ number: 1, repository: 'org/repo' })
      const pr2 = makePR({ number: 2, repository: 'org/repo' })
      // Same PR in both lists — should appear once
      const shared = makePR({ number: 1, repository: 'org/repo' })
      reviewStore.createdByMe = [pr1, pr2]
      reviewStore.reviewRequested = [shared]

      const all = reviewStore.allPRs
      expect(all).toHaveLength(2)
      expect(all.map((p) => p.number)).toEqual([1, 2])
    })

    it('returns empty when both lists are empty', () => {
      expect(reviewStore.allPRs).toHaveLength(0)
    })

    it('preserves order: createdByMe first, then reviewRequested', () => {
      reviewStore.createdByMe = [makePR({ number: 10, repository: 'org/a' })]
      reviewStore.reviewRequested = [makePR({ number: 20, repository: 'org/b' })]

      const all = reviewStore.allPRs
      expect(all[0].number).toBe(10)
      expect(all[1].number).toBe(20)
    })
  })

  describe('byRepo', () => {
    it('returns PRs for matching repository', () => {
      reviewStore.createdByMe = [
        makePR({ number: 1, repository: 'org/repo-a' }),
        makePR({ number: 2, repository: 'org/repo-b' }),
      ]

      expect(reviewStore.byRepo('org/repo-a')).toHaveLength(1)
      expect(reviewStore.byRepo('org/repo-a')[0].number).toBe(1)
    })

    it('returns empty for unknown repo', () => {
      reviewStore.createdByMe = [makePR({ number: 1, repository: 'org/repo' })]
      expect(reviewStore.byRepo('org/other')).toHaveLength(0)
    })
  })

  describe('byTask', () => {
    it('returns empty when task has no projectId', () => {
      reviewStore.createdByMe = [makePR({ number: 1, repository: 'org/repo' })]
      expect(reviewStore.byTask({})).toHaveLength(0)
    })

    it('matches by prNumber when available', () => {
      reviewStore.createdByMe = [
        makePR({ number: 1, repository: 'org/repo' }),
        makePR({ number: 2, repository: 'org/repo' }),
      ]

      const result = reviewStore.byTask({ projectId: 'org/repo', prNumber: 1 })
      expect(result).toHaveLength(1)
      expect(result[0].number).toBe(1)
    })

    it('falls back to branch match when prNumber not found', () => {
      reviewStore.createdByMe = [
        makePR({ number: 5, repository: 'org/repo', headRefName: 'feature/x' }),
      ]

      const result = reviewStore.byTask({ projectId: 'org/repo', prNumber: 999, branch: 'feature/x' })
      expect(result).toHaveLength(1)
      expect(result[0].headRefName).toBe('feature/x')
    })

    it('matches by branch when no prNumber given', () => {
      reviewStore.createdByMe = [
        makePR({ number: 5, repository: 'org/repo', headRefName: 'fix/bug' }),
      ]

      const result = reviewStore.byTask({ projectId: 'org/repo', branch: 'fix/bug' })
      expect(result).toHaveLength(1)
    })

    it('returns empty when neither prNumber nor branch match', () => {
      reviewStore.createdByMe = [
        makePR({ number: 1, repository: 'org/repo', headRefName: 'main' }),
      ]

      const result = reviewStore.byTask({ projectId: 'org/repo', prNumber: 99, branch: 'other' })
      expect(result).toHaveLength(0)
    })
  })

  describe('event listener', () => {
    it('updates state from reviews:updated event', () => {
      reviewStore.listen()

      const cb = eventCallbacks[ReviewsUpdated]
      expect(cb).toBeDefined()

      cb({ createdByMe: [makePR({ number: 10 })], reviewRequested: [makePR({ number: 20 })] })

      expect(reviewStore.createdByMe).toHaveLength(1)
      expect(reviewStore.createdByMe[0].number).toBe(10)
      expect(reviewStore.reviewRequested).toHaveLength(1)
      expect(reviewStore.reviewRequested[0].number).toBe(20)
    })

    it('handles null in event data', () => {
      reviewStore.listen()
      eventCallbacks[ReviewsUpdated]({ createdByMe: null, reviewRequested: null })

      expect(reviewStore.createdByMe).toHaveLength(0)
      expect(reviewStore.reviewRequested).toHaveLength(0)
    })

    it('stopListening removes callback', () => {
      reviewStore.listen()
      expect(eventCallbacks[ReviewsUpdated]).toBeDefined()

      reviewStore.stopListening()
      expect(eventCallbacks[ReviewsUpdated]).toBeUndefined()
    })
  })
})
