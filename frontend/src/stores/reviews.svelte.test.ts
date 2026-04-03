import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

const mockFetchReviews = vi.fn()

vi.mock('../../wailsjs/go/main/App.js', () => ({
  FetchReviews: (...args: unknown[]) => mockFetchReviews(...args),
}))

const { reviewStore } = await import('./reviews.svelte.js')

function makePR(overrides: Record<string, unknown> = {}) {
  return {
    number: 1,
    title: 'Test PR',
    url: 'https://github.com/org/repo/pull/1',
    repository: 'org/repo',
    repoName: 'repo',
    author: 'user',
    isDraft: false,
    labels: [],
    ciStatus: '',
    reviewDecision: '',
    unresolvedCount: 0,
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  }
}

describe('ReviewStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    reviewStore.createdByMe = []
    reviewStore.reviewRequested = []
    reviewStore.error = ''
    reviewStore.loading = false
    reviewStore.stopPolling()
  })

  afterEach(() => {
    reviewStore.stopPolling()
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
      reviewStore.createdByMe = [makePR({ number: 1 }), makePR({ number: 2 })] as any
      reviewStore.reviewRequested = [makePR({ number: 3 })] as any

      expect(reviewStore.totalCount).toBe(3)
    })

    it('returns 0 when empty', () => {
      expect(reviewStore.totalCount).toBe(0)
    })
  })

  describe('polling', () => {
    it('polls at 60s interval', () => {
      vi.useFakeTimers()
      mockFetchReviews.mockResolvedValue({ createdByMe: [], reviewRequested: [] })

      reviewStore.startPolling()

      vi.advanceTimersByTime(60_000)
      expect(mockFetchReviews).toHaveBeenCalledTimes(1)

      vi.advanceTimersByTime(60_000)
      expect(mockFetchReviews).toHaveBeenCalledTimes(2)

      reviewStore.stopPolling()
      vi.advanceTimersByTime(120_000)
      expect(mockFetchReviews).toHaveBeenCalledTimes(2)

      vi.useRealTimers()
    })

    it('replaces existing timer on restart', () => {
      vi.useFakeTimers()
      mockFetchReviews.mockResolvedValue({ createdByMe: [], reviewRequested: [] })

      reviewStore.startPolling()
      reviewStore.startPolling()

      vi.advanceTimersByTime(60_000)
      expect(mockFetchReviews).toHaveBeenCalledTimes(1)

      reviewStore.stopPolling()
      vi.useRealTimers()
    })
  })
})
