import { describe, it, expect, vi, beforeEach } from 'vitest'
import { LoopAgent } from '../../bindings/github.com/Automaat/sybra/internal/loopagent/models.js'

const mockListLoopAgents = vi.fn()
const mockGetLoopAgent = vi.fn()
const mockCreateLoopAgent = vi.fn()
const mockUpdateLoopAgent = vi.fn()
const mockDeleteLoopAgent = vi.fn()
const mockRunLoopAgentNow = vi.fn()
const mockListLoopAgentRuns = vi.fn()

vi.mock('$lib/api', () => ({
  ListLoopAgents: (...args: unknown[]) => mockListLoopAgents(...args),
  GetLoopAgent: (...args: unknown[]) => mockGetLoopAgent(...args),
  CreateLoopAgent: (...args: unknown[]) => mockCreateLoopAgent(...args),
  UpdateLoopAgent: (...args: unknown[]) => mockUpdateLoopAgent(...args),
  DeleteLoopAgent: (...args: unknown[]) => mockDeleteLoopAgent(...args),
  RunLoopAgentNow: (...args: unknown[]) => mockRunLoopAgentNow(...args),
  ListLoopAgentRuns: (...args: unknown[]) => mockListLoopAgentRuns(...args),
}))

const { loopStore } = await import('./loops.svelte.js')

function makeLoop(overrides: Record<string, unknown> = {}): LoopAgent {
  return LoopAgent.createFrom({
    id: 'loop-1',
    name: 'nightly-monitor',
    prompt: 'Check for issues',
    intervalSec: 3600,
    allowedTools: [],
    provider: 'anthropic',
    model: 'claude-sonnet-4-6',
    enabled: true,
    lastRunId: '',
    lastRunCost: 0,
    ...overrides,
  })
}

describe('LoopStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    loopStore.items = new Map()
    loopStore.error = ''
    loopStore.loading = false
    loopStore.stopPolling()
  })

  describe('load', () => {
    it('fetches loop agents from backend', async () => {
      const loops = [makeLoop({ id: 'l1' }), makeLoop({ id: 'l2' })]
      mockListLoopAgents.mockResolvedValue(loops)

      await loopStore.load()

      expect(mockListLoopAgents).toHaveBeenCalled()
      expect(loopStore.items.size).toBe(2)
    })

    it('handles null result', async () => {
      mockListLoopAgents.mockResolvedValue(null)

      await loopStore.load()

      expect(loopStore.items.size).toBe(0)
      expect(loopStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockListLoopAgents.mockRejectedValue(new Error('load failed'))

      await loopStore.load()

      expect(loopStore.error).toBe('Error: load failed')
    })
  })

  describe('list', () => {
    it('sorts loops by name alphabetically', () => {
      loopStore.items = new Map([
        ['z', makeLoop({ id: 'z', name: 'zebra' })],
        ['a', makeLoop({ id: 'a', name: 'alpha' })],
        ['m', makeLoop({ id: 'm', name: 'monitor' })],
      ])

      const list = loopStore.list
      expect(list[0].name).toBe('alpha')
      expect(list[1].name).toBe('monitor')
      expect(list[2].name).toBe('zebra')
    })
  })

  describe('get', () => {
    it('fetches loop agent by id', async () => {
      const loop = makeLoop({ id: 'l1', name: 'fetched' })
      mockGetLoopAgent.mockResolvedValue(loop)

      const result = await loopStore.get('l1')

      expect(mockGetLoopAgent).toHaveBeenCalledWith('l1')
      expect(result.name).toBe('fetched')
      expect(loopStore.items.get('l1')).toBeDefined()
    })
  })

  describe('create', () => {
    it('creates loop agent and adds to map', async () => {
      const loop = makeLoop({ id: 'new-1', name: 'new-monitor' })
      mockCreateLoopAgent.mockResolvedValue(loop)

      const result = await loopStore.create({ name: 'new-monitor', prompt: 'Watch things' })

      expect(result.id).toBe('new-1')
      expect(loopStore.items.get('new-1')).toBeDefined()
    })
  })

  describe('update', () => {
    it('updates loop agent in map', async () => {
      const original = makeLoop({ id: 'l1', intervalSec: 3600 })
      loopStore.items.set('l1', original)
      const updated = makeLoop({ id: 'l1', intervalSec: 7200 })
      mockUpdateLoopAgent.mockResolvedValue(updated)

      const result = await loopStore.update(makeLoop({ id: 'l1', intervalSec: 7200 }))

      expect(result.intervalSec).toBe(7200)
      expect(loopStore.items.get('l1')!.intervalSec).toBe(7200)
    })
  })

  describe('remove', () => {
    it('deletes loop agent and removes from map', async () => {
      loopStore.items.set('l1', makeLoop({ id: 'l1' }))
      mockDeleteLoopAgent.mockResolvedValue(undefined)

      await loopStore.remove('l1')

      expect(mockDeleteLoopAgent).toHaveBeenCalledWith('l1')
      expect(loopStore.items.has('l1')).toBe(false)
    })
  })

  describe('runNow', () => {
    it('triggers run and returns agent id', async () => {
      const loop = makeLoop({ id: 'l1', lastRunId: 'agent-42' })
      mockRunLoopAgentNow.mockResolvedValue('agent-42')
      mockGetLoopAgent.mockResolvedValue(loop)

      const agentId = await loopStore.runNow('l1')

      expect(mockRunLoopAgentNow).toHaveBeenCalledWith('l1')
      expect(agentId).toBe('agent-42')
    })

    it('refreshes loop agent after run', async () => {
      mockRunLoopAgentNow.mockResolvedValue('agent-1')
      const refreshed = makeLoop({ id: 'l1', lastRunId: 'agent-1' })
      mockGetLoopAgent.mockResolvedValue(refreshed)

      await loopStore.runNow('l1')

      expect(mockGetLoopAgent).toHaveBeenCalledWith('l1')
    })
  })

  describe('runs', () => {
    it('fetches run history', async () => {
      const runs = [{ id: 'r1', loopId: 'l1' }, { id: 'r2', loopId: 'l1' }]
      mockListLoopAgentRuns.mockResolvedValue(runs)

      const result = await loopStore.runs('l1')

      expect(mockListLoopAgentRuns).toHaveBeenCalledWith('l1', 10)
      expect(result).toHaveLength(2)
    })

    it('accepts custom limit', async () => {
      mockListLoopAgentRuns.mockResolvedValue([])

      await loopStore.runs('l1', 5)

      expect(mockListLoopAgentRuns).toHaveBeenCalledWith('l1', 5)
    })

    it('handles null result', async () => {
      mockListLoopAgentRuns.mockResolvedValue(null)

      const result = await loopStore.runs('l1')

      expect(result).toEqual([])
    })
  })
})
