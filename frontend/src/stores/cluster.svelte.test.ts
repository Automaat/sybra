import { describe, it, expect, vi, beforeEach } from 'vitest'

const mockGetNodes = vi.fn()

vi.mock('$lib/api', () => ({
  GetNodes: (...a: unknown[]) => mockGetNodes(...a),
}))

const { clusterStore } = await import('./cluster.svelte.js')

describe('ClusterStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    clusterStore.nodes = []
    clusterStore.error = ''
    clusterStore.stopPolling()
  })

  it('loads the roster and reports enabled/names', async () => {
    mockGetNodes.mockResolvedValue([
      { name: 'pet-box', status: 'online' },
      { name: 'work-box', status: 'offline', lastError: 'refused' },
    ])
    await clusterStore.load()
    expect(clusterStore.enabled).toBe(true)
    expect(clusterStore.names).toEqual(['pet-box', 'work-box'])
    expect(clusterStore.statusOf('pet-box')).toBe('online')
    expect(clusterStore.statusOf('work-box')).toBe('offline')
  })

  it('reports unknown for an unrostered node and empty for none', async () => {
    mockGetNodes.mockResolvedValue([{ name: 'pet-box', status: 'online' }])
    await clusterStore.load()
    expect(clusterStore.statusOf('ghost')).toBe('unknown')
    expect(clusterStore.statusOf(undefined)).toBe('')
  })

  it('is disabled with no followers', async () => {
    mockGetNodes.mockResolvedValue([])
    await clusterStore.load()
    expect(clusterStore.enabled).toBe(false)
  })

  it('captures a load error without throwing', async () => {
    mockGetNodes.mockRejectedValue(new Error('boom'))
    await clusterStore.load()
    expect(clusterStore.error).toContain('boom')
  })
})
