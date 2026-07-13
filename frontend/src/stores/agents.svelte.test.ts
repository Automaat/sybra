import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { Agent, StreamEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'

const mockListAgents = vi.fn()
const mockStartAgent = vi.fn()
const mockStopAgent = vi.fn()
const mockStopAgentOnNode = vi.fn()
const mockGetAgentOutput = vi.fn()
const mockDiscoverAgents = vi.fn()
const mockAgentQueueSnapshot = vi.fn()

vi.mock('$lib/api', () => ({
  StartAgent: (...args: unknown[]) => mockStartAgent(...args),
  ListAgents: (...args: unknown[]) => mockListAgents(...args),
  StopAgent: (...args: unknown[]) => mockStopAgent(...args),
  StopAgentOnNode: (...args: unknown[]) => mockStopAgentOnNode(...args),
  GetAgentOutput: (...args: unknown[]) => mockGetAgentOutput(...args),
  DiscoverAgents: (...args: unknown[]) => mockDiscoverAgents(...args),
  AgentQueueSnapshot: (...args: unknown[]) => mockAgentQueueSnapshot(...args),
}))

const { agentStore } = await import('./agents.svelte.js')

function makeAgent(overrides: Record<string, unknown> = {}): Agent {
  return {
    id: 'test-1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    costUsd: 0,
    startedAt: new Date().toISOString(),
    external: false,
    ...overrides,
  }
}

function makeSnapshotItem(overrides: Record<string, unknown> = {}) {
  return {
    taskId: 'task-1',
    role: 'implementation',
    position: 1,
    depth: 1,
    priority: 'medium',
    effectivePriority: 'medium',
    status: 'todo',
    manual: true,
    mode: 'headless',
    enqueued: '2026-07-11T12:00:00Z',
    ...overrides,
  }
}

function makeSnapshot(items: Array<Record<string, unknown>> = []) {
  return {
    depth: items.length,
    items,
  }
}

describe('AgentStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    agentStore.agents = new Map()
    agentStore.outputs.clear()
    agentStore.stepTexts.clear()
    agentStore.queueByTask = new Map()
    agentStore.error = ''
    agentStore.loading = false
    agentStore.stopPolling()
    mockDiscoverAgents.mockResolvedValue([])
    mockListAgents.mockResolvedValue([])
    mockAgentQueueSnapshot.mockResolvedValue(makeSnapshot())
  })

  afterEach(() => {
    agentStore.stopPolling()
  })

  describe('load', () => {
    it('fetches agents from backend', async () => {
      const agents = [makeAgent({ id: 'a1' }), makeAgent({ id: 'a2', taskId: 'task-2' })]
      mockListAgents.mockResolvedValue(agents)

      await agentStore.load()

      expect(mockDiscoverAgents).toHaveBeenCalled()
      expect(mockListAgents).toHaveBeenCalled()
      expect(agentStore.agents.size).toBe(2)
      expect(agentStore.agents.get('a1')).toBeDefined()
      expect(agentStore.agents.get('a2')).toBeDefined()
    })

    it('hydrates queued agents from the supplemental snapshot', async () => {
      mockAgentQueueSnapshot.mockResolvedValue(makeSnapshot([
        makeSnapshotItem({ taskId: 'task-queued', position: 2, depth: 3 }),
      ]))

      await agentStore.load()

      const queued = agentStore.agents.get('queued-task-queued') as Record<string, unknown> | undefined
      expect(queued).toBeDefined()
      expect(queued?.state).toBe('queued')
      expect(queued?.queuePosition).toBe(2)
      expect(queued?.queueDepth).toBe(3)
      expect(agentStore.queueByTask.get('task-queued')?.position).toBe(2)
    })

    it('prefers real agents over queued rows for the same task', async () => {
      mockListAgents.mockResolvedValue([makeAgent({ id: 'real-1', taskId: 'task-1', state: 'running' })])
      mockAgentQueueSnapshot.mockResolvedValue(makeSnapshot([
        makeSnapshotItem({ taskId: 'task-1', position: 1, depth: 2 }),
        makeSnapshotItem({ taskId: 'task-2', position: 2, depth: 2 }),
      ]))

      await agentStore.load()

      expect(agentStore.agents.get('queued-task-1')).toBeUndefined()
      expect(agentStore.byTask('task-1')?.id).toBe('real-1')
      expect(agentStore.agents.get('queued-task-2')).toBeDefined()
      expect((agentStore.agents.get('real-1') as Record<string, unknown>).queuePosition).toBe(1)
    })

    it('drops stale synthetic queued agents after a successful snapshot omits the task', async () => {
      vi.useFakeTimers()
      mockAgentQueueSnapshot.mockResolvedValue(makeSnapshot([
        makeSnapshotItem({ taskId: 'task-stale' }),
      ]))
      await agentStore.load()
      expect(agentStore.agents.get('queued-task-stale')).toBeDefined()

      mockAgentQueueSnapshot.mockResolvedValue(makeSnapshot())
      await vi.advanceTimersByTimeAsync(600)
      await agentStore.load()

      expect(agentStore.agents.get('queued-task-stale')).toBeUndefined()
      expect(agentStore.queueByTask.get('task-stale')).toBeUndefined()
      vi.useRealTimers()
    })

    it('keeps queued rows and avoids a store error when the snapshot fetch fails', async () => {
      mockAgentQueueSnapshot.mockResolvedValue(makeSnapshot([
        makeSnapshotItem({ taskId: 'task-queued' }),
      ]))
      await agentStore.load()

      mockAgentQueueSnapshot.mockRejectedValue(new Error('queue unavailable'))
      await agentStore.load()

      expect(agentStore.error).toBe('')
      expect(agentStore.agents.get('queued-task-queued')).toBeDefined()
      expect(agentStore.queueByTask.get('task-queued')?.position).toBe(1)
    })

    it('handles null result', async () => {
      mockDiscoverAgents.mockResolvedValue(null)
      mockListAgents.mockResolvedValue(null)

      await agentStore.load()

      expect(agentStore.agents.size).toBe(0)
      expect(agentStore.error).toBe('')
    })

    it('sets error on primary load failure', async () => {
      mockDiscoverAgents.mockRejectedValue(new Error('network error'))

      await agentStore.load()

      expect(agentStore.error).toBe('Error: network error')
    })

    it('sets loading flag', async () => {
      const promise = agentStore.load()
      expect(agentStore.loading).toBe(true)
      await promise
      expect(agentStore.loading).toBe(false)
    })
  })

  describe('start', () => {
    it('calls StartAgent and adds to map', async () => {
      const agent = makeAgent({ id: 'new-1' })
      mockStartAgent.mockResolvedValue(agent)

      const result = await agentStore.start('task-1', 'headless', 'do stuff', false)

      expect(mockStartAgent).toHaveBeenCalledWith('task-1', 'headless', 'do stuff', false)
      expect(result.id).toBe('new-1')
      expect(agentStore.agents.get('new-1')).toBeDefined()
      expect(agentStore.outputs.get('new-1')).toEqual([])
    })

    it('keeps a queued start successful even when queue refresh fails', async () => {
      const queued = makeAgent({ id: 'queued-task-1', taskId: 'task-1', state: 'queued' })
      mockStartAgent.mockResolvedValue(queued)
      mockAgentQueueSnapshot.mockRejectedValue(new Error('snapshot down'))

      await expect(agentStore.start('task-1', 'headless', 'do stuff', false)).resolves.toMatchObject({
        id: 'queued-task-1',
        state: 'queued',
      })

      expect(agentStore.agents.get('queued-task-1')).toBeDefined()
      expect(agentStore.outputs.get('queued-task-1')).toEqual([])
      expect(agentStore.error).toBe('')
    })
  })

  describe('stop', () => {
    it('calls StopAgent and updates state', async () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }))
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

      expect(result).toHaveLength(1)
      expect(result[0].event).toEqual(events[0])
      expect(result[0].receivedAt).toBeInstanceOf(Date)
      const stored = agentStore.outputs.get('a1')!
      expect(stored).toHaveLength(1)
      expect(stored[0].event).toEqual(events[0])
    })

    it('handles null result', async () => {
      mockGetAgentOutput.mockResolvedValue(null)

      const result = await agentStore.getOutput('a1')

      expect(result).toEqual([])
      expect(agentStore.outputs.get('a1')).toEqual([])
    })

    it('seeds stepTexts from last extractable event in history', async () => {
      const events = [
        { type: 'init', content: '' },
        { type: 'assistant', content: 'hello\n[Read] reading file' },
        { type: 'tool_result', content: 'done' },
      ]
      mockGetAgentOutput.mockResolvedValue(events)

      await agentStore.getOutput('a1')

      expect(agentStore.stepTexts.get('a1')).toBe('[Read] reading file')
    })

    it('does not set stepTexts when no extractable event in history', async () => {
      const events = [
        { type: 'init', content: '' },
        { type: 'tool_result', content: 'done' },
      ]
      mockGetAgentOutput.mockResolvedValue(events)

      await agentStore.getOutput('a1')

      expect(agentStore.stepTexts.get('a1')).toBeUndefined()
    })
  })

  describe('appendEvent', () => {
    it('appends to existing output', () => {
      agentStore.outputs.set('a1', [{ event: { type: 'init', content: 'start' } as unknown as StreamEvent, receivedAt: new Date() }])
      agentStore.appendEvent('a1', { type: 'assistant', content: 'hi' } as unknown as StreamEvent)

      const events = agentStore.outputs.get('a1')!
      expect(events).toHaveLength(2)
      expect(events[1].event.type).toBe('assistant')
    })

    it('creates new array if none exists', () => {
      agentStore.appendEvent('a1', { type: 'init', content: '' } as unknown as StreamEvent)

      expect(agentStore.outputs.get('a1')).toHaveLength(1)
    })
  })

  describe('updateAgent', () => {
    it('updates agent in map', () => {
      const agent = makeAgent({ id: 'a1', state: 'running' })
      agentStore.agents.set('a1', agent)

      const updated = makeAgent({ id: 'a1', state: 'stopped' })
      agentStore.updateAgent('a1', updated)

      expect(agentStore.agents.get('a1')!.state).toBe('stopped')
    })
  })

  describe('list', () => {
    it('returns agents sorted by startedAt descending', () => {
      agentStore.agents.set('old', makeAgent({
        id: 'old',
        startedAt: '2026-01-01T00:00:00Z',
      }))
      agentStore.agents.set('new', makeAgent({
        id: 'new',
        startedAt: '2026-04-01T00:00:00Z',
      }))

      const list = agentStore.list
      expect(list[0].id).toBe('new')
      expect(list[1].id).toBe('old')
    })
  })

  describe('byTask', () => {
    it('finds agent by task ID', () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1', taskId: 'task-42' }))
      agentStore.agents.set('a2', makeAgent({ id: 'a2', taskId: 'task-99' }))

      const found = agentStore.byTask('task-42')
      expect(found?.id).toBe('a1')
    })

    it('returns undefined when not found', () => {
      expect(agentStore.byTask('nonexistent')).toBeUndefined()
    })
  })

  describe('byState', () => {
    it('filters by state', () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1', state: 'running' }))
      agentStore.agents.set('a2', makeAgent({ id: 'a2', state: 'idle' }))
      agentStore.agents.set('a3', makeAgent({ id: 'a3', state: 'running' }))

      expect(agentStore.byState('running')).toHaveLength(2)
      expect(agentStore.byState('idle')).toHaveLength(1)
      expect(agentStore.byState('stopped')).toHaveLength(0)
    })

    it('returns all for "all" filter', () => {
      agentStore.agents.set('a1', makeAgent({ id: 'a1' }))
      agentStore.agents.set('a2', makeAgent({ id: 'a2' }))

      expect(agentStore.byState('all')).toHaveLength(2)
    })
  })

  describe('polling', () => {
    it('starts and stops interval', async () => {
      vi.useFakeTimers()

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

      agentStore.startPolling(5000)
      agentStore.startPolling(5000)

      await vi.advanceTimersByTimeAsync(5000)
      expect(mockDiscoverAgents).toHaveBeenCalledTimes(1)

      agentStore.stopPolling()
      vi.useRealTimers()
    })
  })
})

describe('AgentStore node-aware stop routing', () => {
  it('stops a homed-away agent on its follower, not the leader', async () => {
    const { taskStore } = await import('./tasks.svelte.js')
    vi.clearAllMocks()
    agentStore.items = new Map()
    taskStore.tasks = new Map()
    agentStore.items.set('a1', { id: 'a1', taskId: 't1', state: 'running' } as never)
    taskStore.tasks.set('t1', { id: 't1', assignedNode: 'pet-box' } as never)

    await agentStore.stop('a1')

    expect(mockStopAgentOnNode).toHaveBeenCalledWith('pet-box', 'a1')
    expect(mockStopAgent).not.toHaveBeenCalled()
  })

  it('stops a local agent on the leader', async () => {
    const { taskStore } = await import('./tasks.svelte.js')
    vi.clearAllMocks()
    agentStore.items = new Map()
    taskStore.tasks = new Map()
    agentStore.items.set('a2', { id: 'a2', taskId: 't2', state: 'running' } as never)
    taskStore.tasks.set('t2', { id: 't2' } as never)

    await agentStore.stop('a2')

    expect(mockStopAgent).toHaveBeenCalledWith('a2')
    expect(mockStopAgentOnNode).not.toHaveBeenCalled()
  })
})
