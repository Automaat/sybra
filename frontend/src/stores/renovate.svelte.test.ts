import { describe, it, expect, vi, beforeEach } from 'vitest'
import { RenovatePR } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'
import { RenovateUpdated } from '../lib/events.js'

const mockFetchRenovatePRs = vi.fn()
let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('$lib/api', () => ({
  FetchRenovatePRs: (...args: unknown[]) => mockFetchRenovatePRs(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
}))

const { renovateStore } = await import('./renovate.svelte.js')

function makePR(overrides: Record<string, unknown> = {}): RenovatePR {
  return RenovatePR.createFrom({
    number: 1,
    title: 'chore(deps): update dependency',
    url: 'https://github.com/org/repo/pull/1',
    repository: 'org/repo',
    repoName: 'repo',
    author: 'renovate[bot]',
    isDraft: false,
    labels: [],
    headRefName: 'renovate/update',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: '',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    checkRuns: [],
    ...overrides,
  })
}

describe('RenovateStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    eventCallbacks = {}
    renovateStore.prs = []
    renovateStore.error = ''
    renovateStore.loading = false
    renovateStore.stopListening()
  })

  describe('load', () => {
    it('fetches PRs from backend', async () => {
      const prs = [makePR({ number: 1 }), makePR({ number: 2 })]
      mockFetchRenovatePRs.mockResolvedValue(prs)

      await renovateStore.load()

      expect(mockFetchRenovatePRs).toHaveBeenCalled()
      expect(renovateStore.prs).toHaveLength(2)
    })

    it('handles null result', async () => {
      mockFetchRenovatePRs.mockResolvedValue(null)

      await renovateStore.load()

      expect(renovateStore.prs).toHaveLength(0)
      expect(renovateStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockFetchRenovatePRs.mockRejectedValue(new Error('gh not found'))

      await renovateStore.load()

      expect(renovateStore.error).toBe('Error: gh not found')
    })

    it('sets loading flag', async () => {
      mockFetchRenovatePRs.mockResolvedValue([])

      const promise = renovateStore.load()
      expect(renovateStore.loading).toBe(true)
      await promise
      expect(renovateStore.loading).toBe(false)
    })

    it('clears error on success', async () => {
      renovateStore.error = 'old error'
      mockFetchRenovatePRs.mockResolvedValue([])

      await renovateStore.load()

      expect(renovateStore.error).toBe('')
    })
  })

  describe('count', () => {
    it('returns total number of PRs', () => {
      renovateStore.prs = [makePR({ number: 1 }), makePR({ number: 2 })]
      expect(renovateStore.count).toBe(2)
    })

    it('returns 0 when empty', () => {
      expect(renovateStore.count).toBe(0)
    })
  })

  describe('eligible', () => {
    it.each([
      { isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', expected: true },
      { isDraft: false, mergeable: 'MERGEABLE', ciStatus: '', expected: true },
      { isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', waitingForStability: true, expected: false },
      { isDraft: true, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', expected: false },
      { isDraft: false, mergeable: 'CONFLICTING', ciStatus: 'SUCCESS', expected: false },
      { isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'FAILURE', expected: false },
      { isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'PENDING', expected: false },
    ])('isDraft=$isDraft mergeable=$mergeable ciStatus=$ciStatus waitingForStability=$waitingForStability → $expected', ({ isDraft, mergeable, ciStatus, waitingForStability, expected }) => {
      renovateStore.prs = [makePR({ number: 1, isDraft, mergeable, ciStatus, waitingForStability })]
      expect(renovateStore.eligible).toHaveLength(expected ? 1 : 0)
    })

    it('filters only eligible PRs from mixed list', () => {
      renovateStore.prs = [
        makePR({ number: 1, isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS' }),
        makePR({ number: 2, isDraft: true, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS' }),
        makePR({ number: 3, isDraft: false, mergeable: 'MERGEABLE', ciStatus: '' }),
        makePR({ number: 4, isDraft: false, mergeable: 'MERGEABLE', ciStatus: 'SUCCESS', waitingForStability: true }),
      ]
      expect(renovateStore.eligible).toHaveLength(2)
      expect(renovateStore.eligible.map((p) => p.number)).toEqual([1, 3])
    })
  })

  describe('failing', () => {
    it('returns PRs with FAILURE ci status', () => {
      renovateStore.prs = [
        makePR({ number: 1, ciStatus: 'FAILURE' }),
        makePR({ number: 2, ciStatus: 'SUCCESS' }),
        makePR({ number: 3, ciStatus: 'FAILURE' }),
      ]
      expect(renovateStore.failing).toHaveLength(2)
    })

    it('returns empty when no failures', () => {
      renovateStore.prs = [makePR({ number: 1, ciStatus: 'SUCCESS' })]
      expect(renovateStore.failing).toHaveLength(0)
    })
  })

  describe('listen', () => {
    it('registers event listener', () => {
      renovateStore.listen()

      expect(eventCallbacks[RenovateUpdated]).toBeDefined()
      renovateStore.stopListening()
    })

    it('updates PRs from event', () => {
      renovateStore.listen()

      eventCallbacks[RenovateUpdated]([makePR({ number: 99 })])

      expect(renovateStore.prs).toHaveLength(1)
      expect(renovateStore.prs[0].number).toBe(99)
      renovateStore.stopListening()
    })

    it('handles null in event data', () => {
      renovateStore.listen()

      eventCallbacks[RenovateUpdated](null)

      expect(renovateStore.prs).toHaveLength(0)
      renovateStore.stopListening()
    })
  })

  describe('stopListening', () => {
    it('removes event listener', () => {
      renovateStore.listen()
      renovateStore.stopListening()

      expect(eventCallbacks[RenovateUpdated]).toBeUndefined()
    })

    it('is safe to call when not listening', () => {
      expect(() => renovateStore.stopListening()).not.toThrow()
    })
  })
})
