import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ReviewsUpdated } from '../lib/events.js'

const mockFetchReviews = vi.fn()
let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('../../wailsjs/go/main/App.js', () => ({
  FetchReviews: (...args: unknown[]) => mockFetchReviews(...args),
}))

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
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
    mergeable: '',
    unresolvedCount: 0,
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  }
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
      reviewStore.createdByMe = [makePR({ number: 1 }), makePR({ number: 2 })] as any
      reviewStore.reviewRequested = [makePR({ number: 3 })] as any

      expect(reviewStore.totalCount).toBe(3)
    })

    it('returns 0 when empty', () => {
      expect(reviewStore.totalCount).toBe(0)
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
