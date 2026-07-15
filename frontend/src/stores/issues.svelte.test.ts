import { describe, it, expect, vi, beforeEach } from 'vitest'
import { Issue } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'
import { IssuesUpdated } from '../lib/events.js'

const mockFetchAssignedIssues = vi.fn()
let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('$lib/api', () => ({
  FetchAssignedIssues: (...args: unknown[]) => mockFetchAssignedIssues(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
}))

const { issueStore } = await import('./issues.svelte.js')

function makeIssue(overrides: Record<string, unknown> = {}): Issue {
  return Issue.createFrom({
    number: 1,
    title: 'Bug report',
    body: 'Description',
    url: 'https://github.com/org/repo/issues/1',
    repository: 'org/repo',
    repoName: 'repo',
    labels: [],
    author: 'user',
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  })
}

describe('IssueStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    eventCallbacks = {}
    issueStore.issues = []
    issueStore.error = ''
    issueStore.loading = false
    issueStore.stopListening()
  })

  describe('load', () => {
    it('fetches issues from backend', async () => {
      const issues = [makeIssue({ number: 1 }), makeIssue({ number: 2 })]
      mockFetchAssignedIssues.mockResolvedValue(issues)

      await issueStore.load()

      expect(mockFetchAssignedIssues).toHaveBeenCalled()
      expect(issueStore.issues).toHaveLength(2)
    })

    it('handles null result', async () => {
      mockFetchAssignedIssues.mockResolvedValue(null)

      await issueStore.load()

      expect(issueStore.issues).toHaveLength(0)
      expect(issueStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockFetchAssignedIssues.mockRejectedValue(new Error('gh error'))

      await issueStore.load()

      expect(issueStore.error).toBe('Error: gh error')
    })

    it('sets loading flag', async () => {
      mockFetchAssignedIssues.mockResolvedValue([])

      const promise = issueStore.load()
      expect(issueStore.loading).toBe(true)
      await promise
      expect(issueStore.loading).toBe(false)
    })

    it('clears error on success', async () => {
      issueStore.error = 'old error'
      mockFetchAssignedIssues.mockResolvedValue([])

      await issueStore.load()

      expect(issueStore.error).toBe('')
    })
  })

  describe('count', () => {
    it('returns number of issues', () => {
      issueStore.issues = [makeIssue({ number: 1 }), makeIssue({ number: 2 })]
      expect(issueStore.count).toBe(2)
    })

    it('returns 0 when empty', () => {
      expect(issueStore.count).toBe(0)
    })
  })

  describe('listen', () => {
    it('registers event listener', () => {
      issueStore.listen()

      expect(eventCallbacks[IssuesUpdated]).toBeDefined()
      issueStore.stopListening()
    })

    it('updates issues from event', () => {
      issueStore.listen()

      eventCallbacks[IssuesUpdated]([makeIssue({ number: 42 })])

      expect(issueStore.issues).toHaveLength(1)
      expect(issueStore.issues[0].number).toBe(42)
      issueStore.stopListening()
    })

    it('handles null in event data', () => {
      issueStore.listen()

      eventCallbacks[IssuesUpdated](null)

      expect(issueStore.issues).toHaveLength(0)
      issueStore.stopListening()
    })

    it('replaces prior listener on double-listen', () => {
      issueStore.listen()
      issueStore.listen()

      const keys = Object.keys(eventCallbacks).filter((k) => k === IssuesUpdated)
      expect(keys).toHaveLength(1)
      issueStore.stopListening()
    })
  })

  describe('stopListening', () => {
    it('removes event listener', () => {
      issueStore.listen()
      issueStore.stopListening()

      expect(eventCallbacks[IssuesUpdated]).toBeUndefined()
    })

    it('is safe to call when not listening', () => {
      expect(() => issueStore.stopListening()).not.toThrow()
    })
  })
})
