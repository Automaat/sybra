import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

// Mock Wails bindings
const mockListAgents = vi.fn()
const mockStartAgent = vi.fn()
const mockStopAgent = vi.fn()
const mockGetAgentOutput = vi.fn()
const mockDiscoverAgents = vi.fn()

vi.mock('../../wailsjs/go/main/App.js', () => ({
  ListAgents: (...args: unknown[]) => mockListAgents(...args),
  StartAgent: (...args: unknown[]) => mockStartAgent(...args),
  StopAgent: (...args: unknown[]) => mockStopAgent(...args),
  GetAgentOutput: (...args: unknown[]) => mockGetAgentOutput(...args),
  DiscoverAgents: (...args: unknown[]) => mockDiscoverAgents(...args),
}))

// Must import after mock setup
const { agentStore } = await import('./agents.svelte.js')

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'test-1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    tmuxSession: '',
    costUsd: 0,
    startedAt: new Date().toISOString(),
    external: false,
    ...overrides,
  }
}

describe('AgentStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    // Reset store state
    agentStore.agents = new Map()
    agentStore.outputs = new Map()
    agentStore.error = ''
    agentStore.loading = false
    agentStore.stopPolling()
  })

  afterEach(() => {
    agentStore.stopPolling()
  })

  describe('load', () => {
    it('fetches agents from backend', async () => {
      const agents = [makeAgent({ id: 'a1' }), makeAgent({ id: 'a2' })]
      mockDiscoverAgents.mockResolvedValue([])
      mockListAgents.mockResolvedValue(agents)

      await agentStore.load()

      expect(mockDiscoverAgents).toHaveBeenCalled()
      expect(mockListAgents).toHaveBeenCalled()
      expect(agentStore.agents.size).toBe(2)
      expect(agentStore.agents.get('a1')).toBeDefined()
      expect(agentStore.agents.get('a2')).toBeDefined()
    })

    it('handles null result', async () => {
      mockDiscoverAgents.mockResolvedValue(null)
      mockListAgents.mockResolvedValue(null)

      await agentStore.load()

      expect(agentStore.agents.size).toBe(0)
      expect(agentStore.error).toBe('')
    })

    it('sets error on failure', async () => {
      mockDiscoverAgents.mockRejectedValue(new Error('network error'))

      await agentStore.load()

      expect(agentStore.error).toBe('Error: network error')
    })

    it('sets loading flag', async () => {
      mockDiscoverAgents.mockResolvedValue([])
      mockListAgents.mockResolvedValue([])

      const promise = agentStore.load()
      // loading is set synchronously before await
      expect(agentStore.loading).toBe(true)
      await promise
      expect(agentStore.loading).toBe(false)
    })
  })

  describe('start', () => {
    it('calls StartAgent and adds to map', async () => {
      const agent = makeAgent({ id: 'new-1' })
      mockStartAgent.mockResolvedValue(agent)

      const result = await agentStore.start('task-1', 'headless', 'do stuff')

      expect(mockStartAgent).toHaveBeenCalledWith('task-1', 'headless', 'do stuff')
      expect(result.id).toBe('new-1')
      expect(agentStore.agents.get('new-1')).toBeDefined()
      expect(agentStore.outputs.get('new-1')).toEqual([])
    })
  })

  describe('stop', () => {
    it('calls StopAgent and updates state', async () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }) as any)
      mockStopAgent.mockResolvedValue(undefined)

      await agentStore.stop('a1')

      expect(mockStopAgent).toHaveBeenCalledWith('a1')
      expect(agentStore.agents.get('a1')!.state).toBe('stopped')
    })
  })

  describe('getOutput', () => {
    it('fetches and stores output', async () => {
      const events = [{ type: 'assistant', content: 'hello' }]
      mockGetAgentOutput.mockResolvedValue(events)

      const result = await agentStore.getOutput('a1')

      expect(result).toEqual(events)
      expect(agentStore.outputs.get('a1')).toEqual(events)
    })

    it('handles null result', async () => {
      mockGetAgentOutput.mockResolvedValue(null)

      const result = await agentStore.getOutput('a1')

      expect(result).toEqual([])
      expect(agentStore.outputs.get('a1')).toEqual([])
    })
  })

  describe('appendEvent', () => {
    it('appends to existing output', () => {
      agentStore.outputs.set('a1', [{ type: 'init', content: 'start' } as any])
      agentStore.appendEvent('a1', { type: 'assistant', content: 'hi' } as any)

      const events = agentStore.outputs.get('a1')!
      expect(events).toHaveLength(2)
      expect(events[1].type).toBe('assistant')
    })

    it('creates new array if none exists', () => {
      agentStore.appendEvent('a1', { type: 'init', content: '' } as any)

      expect(agentStore.outputs.get('a1')).toHaveLength(1)
    })
  })

  describe('updateAgent', () => {
    it('updates agent in map', () => {
      const agent = makeAgent({ id: 'a1', state: 'running' })
      agentStore.agents.set('a1', agent as any)

      const updated = makeAgent({ id: 'a1', state: 'stopped' })
      agentStore.updateAgent('a1', updated as any)

      expect(agentStore.agents.get('a1')!.state).toBe('stopped')
    })
  })

  describe('list', () => {
    it('returns agents sorted by startedAt descending', () => {
      agentStore.agents.set('old', makeAgent({
        id: 'old',
        startedAt: '2026-01-01T00:00:00Z',
      }) as any)
      agentStore.agents.set('new', makeAgent({
        id: 'new',
        startedAt: '2026-04-01T00:00:00Z',
      }) as any)

      const list = agentStore.list
      expect(list[0].id).toBe('new')
      expect(list[1].id).toBe('old')
    })
  })

  describe('byTask', () => {
    it('finds agent by task ID', () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1', taskId: 'task-42' }) as any)
      agentStore.agents.set('a2', makeAgent({ id: 'a2', taskId: 'task-99' }) as any)

      const found = agentStore.byTask('task-42')
      expect(found?.id).toBe('a1')
    })

    it('returns undefined when not found', () => {
      expect(agentStore.byTask('nonexistent')).toBeUndefined()
    })
  })

  describe('byState', () => {
    it('filters by state', () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }) as any)
      agentStore.agents.set('a2', makeAgent({ id: 'a2', state: 'idle' }) as any)
      agentStore.agents.set('a3', makeAgent({ id: 'a3', state: 'running' }) as any)

      expect(agentStore.byState('running')).toHaveLength(2)
      expect(agentStore.byState('idle')).toHaveLength(1)
      expect(agentStore.byState('stopped')).toHaveLength(0)
    })

    it('returns all for "all" filter', () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1' }) as any)
      agentStore.agents.set('a2', makeAgent({ id: 'a2' }) as any)

      expect(agentStore.byState('all')).toHaveLength(2)
    })
  })

  describe('polling', () => {
    it('starts and stops interval', async () => {
      vi.useFakeTimers()
      mockDiscoverAgents.mockResolvedValue([])
      mockListAgents.mockResolvedValue([])

      agentStore.startPolling(5000)

      await vi.advanceTimersByTimeAsync(5000)
      expect(mockDiscoverAgents).toHaveBeenCalledTimes(1)

      await vi.advanceTimersByTimeAsync(5000)
      expect(mockDiscoverAgents).toHaveBeenCalledTimes(2)

      agentStore.stopPolling()
      await vi.advanceTimersByTimeAsync(10000)
      expect(mockDiscoverAgents).toHaveBeenCalledTimes(2)

      vi.useRealTimers()
    })

    it('replaces existing timer on restart', async () => {
      vi.useFakeTimers()
      mockDiscoverAgents.mockResolvedValue([])
      mockListAgents.mockResolvedValue([])

      agentStore.startPolling(5000)
      agentStore.startPolling(5000) // should not double up

      await vi.advanceTimersByTimeAsync(5000)
      expect(mockDiscoverAgents).toHaveBeenCalledTimes(1) // not 2

      agentStore.stopPolling()
      vi.useRealTimers()
    })
  })
})
