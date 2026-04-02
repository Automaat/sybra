import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockByState = vi.fn()

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    loading: false,
    error: '',
    byState: (...args: unknown[]) => mockByState(...args),
    list: [],
  },
}))

vi.mock('@skeletonlabs/skeleton-svelte', () => ({
  SegmentedControl: Object.assign(() => {}, {
    Control: () => {},
    Indicator: () => {},
    Item: Object.assign(() => {}, {
      ItemText: () => {},
      ItemHiddenInput: () => {},
    }),
  }),
}))

const { agentStore } = await import('../stores/agents.svelte.js')
const AgentList = (await import('./AgentList.svelte')).default

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'a1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'running',
    sessionId: '',
    tmuxSession: '',
    costUsd: 0,
    startedAt: '2026-04-01T00:00:00Z',
    external: false,
    pid: 0,
    command: '',
    name: '',
    project: 'test',
    ...overrides,
  }
}

describe('AgentList', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    ;(agentStore as any).loading = false
    ;(agentStore as any).error = ''
    mockByState.mockReturnValue([])
  })

  afterEach(() => {
    cleanup()
  })

  it('shows loading message when loading', () => {
    ;(agentStore as any).loading = true
    render(AgentList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Loading agents...')).toBeDefined()
  })

  it('shows error message when error is set', () => {
    ;(agentStore as any).error = 'Failed to fetch agents'
    render(AgentList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Failed to fetch agents')).toBeDefined()
  })

  it('shows empty state when filtered returns empty array', () => {
    mockByState.mockReturnValue([])
    render(AgentList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('No agents')).toBeDefined()
    expect(screen.getByText('Start an agent from a task to see it here')).toBeDefined()
  })

  it('renders agent cards when agents exist', () => {
    const agents = [
      makeAgent({ id: 'a1', state: 'running' }),
      makeAgent({ id: 'a2', state: 'idle' }),
    ]
    mockByState.mockReturnValue(agents)
    render(AgentList, { props: { onselect: vi.fn() } })
    expect(screen.queryByText('No agents')).toBeNull()
    expect(mockByState).toHaveBeenCalledWith('all')
  })
})
