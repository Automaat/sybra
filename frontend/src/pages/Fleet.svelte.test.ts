import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockGetMonitorReport = vi.fn()

type Handler = (data: unknown) => void
const eventHandlers: Record<string, Handler> = {}
const unsubSpies: Record<string, ReturnType<typeof vi.fn>> = {}

const mockEventsOn = vi.fn((channel: string, cb: Handler) => {
  eventHandlers[channel] = cb
  const unsub = vi.fn()
  unsubSpies[channel] = unsub
  return unsub
})

vi.mock('$lib/api', () => ({
  GetMonitorReport: (...args: unknown[]) => mockGetMonitorReport(...args),
  EventsOn: (...args: any[]) => mockEventsOn(...(args as [string, Handler])),
}))

const mockAgentList: any[] = []
const mockStepTexts = new Map<string, string>()

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    get list() {
      return mockAgentList
    },
    stepTexts: mockStepTexts,
  },
}))

const mockTaskList: any[] = []

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    get list() {
      return mockTaskList
    },
    byStatus: (status: string) => mockTaskList.filter((t: any) => t.status === status),
  },
}))

const mockLoad = vi.fn().mockResolvedValue(undefined)

const mockStatsStore: { data: any; error: string; loading: boolean; load: () => Promise<void> } = {
  data: null,
  error: '',
  loading: false,
  load: (...args: unknown[]) => mockLoad(...args),
}

vi.mock('../stores/stats.svelte.js', () => ({
  statsStore: mockStatsStore,
}))

const Fleet = (await import('./Fleet.svelte')).default

function emptyBinding(overrides: Record<string, unknown> = {}) {
  return {
    enabled: true,
    ready: false,
    report: {
      generatedAt: '',
      counts: {},
      anomalies: [],
      remediated: [],
      dispatched: [],
      issuesOpened: 0,
      issuesUpdated: 0,
    },
    ...overrides,
  }
}

describe('Fleet', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockGetMonitorReport.mockResolvedValue(emptyBinding())
    mockAgentList.length = 0
    mockTaskList.length = 0
    mockStepTexts.clear()
    mockStatsStore.data = null
    mockStatsStore.error = ''
    mockStatsStore.loading = false
    for (const k of Object.keys(eventHandlers)) delete eventHandlers[k]
    for (const k of Object.keys(unsubSpies)) delete unsubSpies[k]
  })

  afterEach(() => {
    cleanup()
  })

  it('calls statsStore.load on mount', () => {
    render(Fleet, { props: {} })
    expect(mockLoad).toHaveBeenCalled()
  })

  it('renders populated running agents with step text', async () => {
    mockAgentList.push({ id: 'a1', name: 'impl:task-1', taskId: 'task-1', state: 'running' })
    mockStepTexts.set('a1', 'Reading files…')
    render(Fleet, { props: {} })
    expect(screen.getByText('impl:task-1')).toBeDefined()
    expect(screen.getByText('Reading files…')).toBeDefined()
  })

  it('shows empty state when no agents running', () => {
    render(Fleet, { props: {} })
    expect(screen.getByText('No agents running')).toBeDefined()
  })

  it('computes queue depth and needs-you count from tasks', () => {
    mockTaskList.push(
      { id: 't1', status: 'todo' },
      { id: 't2', status: 'todo' },
      { id: 't3', status: 'human-required' },
    )
    render(Fleet, { props: {} })
    expect(screen.getByText('2')).toBeDefined()
    expect(screen.getByText('1')).toBeDefined()
  })

  it('shows stats unavailable when data is absent and no error', () => {
    render(Fleet, { props: {} })
    expect(screen.getByText('Stats unavailable')).toBeDefined()
  })

  it('shows stale label when a refresh error leaves prior data in place', () => {
    mockStatsStore.data = { today: { totalCostUsd: 3.5 }, limits: { providers: [] } }
    mockStatsStore.error = 'network down'
    render(Fleet, { props: {} })
    expect(screen.getByText(/showing last known data \(stale\)/)).toBeDefined()
    expect(screen.getByText('$3.50')).toBeDefined()
  })

  it('renders live burn and provider limits when data is present', () => {
    mockStatsStore.data = {
      today: { totalCostUsd: 1.25 },
      limits: { providers: [{ provider: 'claude', sessionUsedPercent: 40, quotaLimited: false }] },
    }
    render(Fleet, { props: {} })
    expect(screen.getByText('$1.25')).toBeDefined()
    expect(screen.getByText('Claude')).toBeDefined()
    expect(screen.getByText('40% session')).toBeDefined()
  })

  it('shows empty message when provider limits list is empty', () => {
    mockStatsStore.data = { today: { totalCostUsd: 0 }, limits: { providers: [] } }
    render(Fleet, { props: {} })
    expect(screen.getByText('No provider limits reported')).toBeDefined()
  })

  it('shows monitor disabled when binding reports disabled', async () => {
    mockGetMonitorReport.mockResolvedValue(emptyBinding({ enabled: false }))
    render(Fleet, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('monitor disabled')).toBeDefined()
    })
  })

  it('shows waiting when monitor is enabled but not ready', async () => {
    render(Fleet, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('waiting')).toBeDefined()
    })
  })

  it('updates drift count when a MonitorReport event fires', async () => {
    render(Fleet, { props: {} })
    await vi.waitFor(() => expect(eventHandlers['monitor:report']).toBeDefined())
    eventHandlers['monitor:report']({
      generatedAt: new Date().toISOString(),
      counts: {},
      anomalies: [{ id: 'a1', kind: 'stale' }, { id: 'a2', kind: 'orphan' }],
      remediated: [],
      dispatched: [],
      issuesOpened: 0,
      issuesUpdated: 0,
    })
    await vi.waitFor(() => {
      expect(screen.getByText('2')).toBeDefined()
    })
  })

  it('shows monitor unavailable when GetMonitorReport rejects', async () => {
    mockGetMonitorReport.mockRejectedValue(new Error('boom'))
    render(Fleet, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('monitor unavailable')).toBeDefined()
    })
  })

  it('unsubscribes from MonitorReport on unmount', async () => {
    const { unmount } = render(Fleet, { props: {} })
    await vi.waitFor(() => expect(eventHandlers['monitor:report']).toBeDefined())
    unmount()
    expect(unsubSpies['monitor:report']).toHaveBeenCalled()
  })
})
