import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LearningSummary } from '../lib/events.js'
import type { Digest, Status } from '../../bindings/github.com/Automaat/sybra/internal/learning/models.js'

const mockListDigests = vi.fn()
const mockGetLearningDigestStatus = vi.fn()
const eventCallbacks: Record<string, (data: unknown) => void> = {}
const cancelListener = vi.fn()

vi.mock('$lib/api', () => ({
  ListDigests: (...args: unknown[]) => mockListDigests(...args),
  GetLearningDigestStatus: (...args: unknown[]) => mockGetLearningDigestStatus(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return cancelListener
  },
}))

const { LearningStore } = await import('./learning.svelte.js')

function makeDigest(overrides: Partial<Digest> = {}): Digest {
  return {
    schemaVersion: 1,
    generatedAt: '2026-07-02T10:00:00Z',
    since: '2026-07-02T08:00:00Z',
    until: '2026-07-02T09:00:00Z',
    reportDigest: 'abcdef1234567890',
    worked: ['kept changes small'],
    ...overrides,
  } as Digest
}

function makeStatus(overrides: Partial<Status> = {}): Status {
  return {
    enabled: true,
    nextRun: '2026-07-02T11:00:00Z',
    ...overrides,
  } as Status
}

describe('LearningStore', () => {
  let store: InstanceType<typeof LearningStore>

  beforeEach(() => {
    vi.clearAllMocks()
    for (const key of Object.keys(eventCallbacks)) delete eventCallbacks[key]
    store = new LearningStore()
  })

  it('loads digests and status from the backend', async () => {
    const digest = makeDigest()
    const status = makeStatus()
    mockListDigests.mockResolvedValue([digest])
    mockGetLearningDigestStatus.mockResolvedValue(status)

    await store.load()

    expect(mockListDigests).toHaveBeenCalledTimes(1)
    expect(mockGetLearningDigestStatus).toHaveBeenCalledTimes(1)
    expect(store.digests).toEqual([digest])
    expect(store.status).toEqual(status)
    expect(store.loading).toBe(false)
    expect(store.error).toBe('')
  })

  it('sets error and clears loading when load fails', async () => {
    mockListDigests.mockRejectedValue(new Error('digest read failed'))
    mockGetLearningDigestStatus.mockResolvedValue(makeStatus())

    await store.load()

    expect(store.error).toBe('Error: digest read failed')
    expect(store.loading).toBe(false)
  })

  it('prepends learning summary events and deduplicates by digest key', () => {
    const oldDigest = makeDigest({ reportDigest: 'old', since: '2026-07-02T06:00:00Z', until: '2026-07-02T07:00:00Z' })
    const newDigest = makeDigest({ reportDigest: 'new', since: '2026-07-02T07:00:00Z', until: '2026-07-02T08:00:00Z' })
    store.digests = [oldDigest]

    store.listen()
    eventCallbacks[LearningSummary](newDigest)
    eventCallbacks[LearningSummary](newDigest)

    expect(store.digests).toEqual([newDigest, oldDigest])
  })

  it('ignores malformed learning summary events', () => {
    const digest = makeDigest()
    store.digests = [digest]

    store.listen()
    eventCallbacks[LearningSummary]({ reportDigest: 'bad' })

    expect(store.digests).toEqual([digest])
  })

  it('keeps disabled status distinct from an enabled empty journal', async () => {
    mockListDigests.mockResolvedValue([])
    mockGetLearningDigestStatus.mockResolvedValue(makeStatus({ enabled: false }))

    await store.load()

    expect(store.digests).toEqual([])
    expect(store.status?.enabled).toBe(false)
  })

  it('cancels the active listener', () => {
    store.listen()
    store.stopListening()

    expect(cancelListener).toHaveBeenCalledTimes(1)
  })
})
