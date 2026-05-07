import { describe, it, expect, vi, beforeEach } from 'vitest'

const mockGetStats = vi.fn()

vi.mock('$lib/api', () => ({
  GetStats: (...args: unknown[]) => mockGetStats(...args),
}))

const { statsStore } = await import('./stats.svelte.js')

describe('StatsStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    statsStore.data = null
    statsStore.error = ''
    statsStore.loading = false
  })

  describe('load', () => {
    it('fetches stats from backend', async () => {
      const data = { taskCount: 5, agentCount: 2 }
      mockGetStats.mockResolvedValue(data)

      await statsStore.load()

      expect(mockGetStats).toHaveBeenCalled()
      expect(statsStore.data).toEqual(data)
    })

    it('sets error on failure', async () => {
      mockGetStats.mockRejectedValue(new Error('stats error'))

      await statsStore.load()

      expect(statsStore.error).toBe('Error: stats error')
      expect(statsStore.data).toBeNull()
    })

    it('sets loading flag during fetch', async () => {
      mockGetStats.mockResolvedValue({})

      const promise = statsStore.load()
      expect(statsStore.loading).toBe(true)
      await promise
      expect(statsStore.loading).toBe(false)
    })

    it('clears loading on error', async () => {
      mockGetStats.mockRejectedValue(new Error('fail'))

      await statsStore.load()

      expect(statsStore.loading).toBe(false)
    })

    it('clears error before fetching', async () => {
      statsStore.error = 'old error'
      mockGetStats.mockResolvedValue({})

      await statsStore.load()

      expect(statsStore.error).toBe('')
    })
  })
})
