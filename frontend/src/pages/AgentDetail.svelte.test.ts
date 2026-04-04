import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { agentState } from '../lib/events.js'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockStop = vi.fn()
const mockUpdateAgent = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())

const mockAgents = new Map()

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    agents: mockAgents,
    stop: (...args: unknown[]) => mockStop(...args),
    updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
  },
}))

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

vi.mock('../components/StreamOutput.svelte', () => ({ default: () => {} }))

const AgentDetail = (await import('./AgentDetail.svelte')).default

const mockAgent = {
  id: 'agent-1',
  taskId: 'task-1',
  mode: 'headless',
  state: 'running',
  sessionId: 'sess-123',
  tmuxSession: '',
  costUsd: 0.5678,
  startedAt: '2026-04-01T00:00:00Z',
  external: true,
  pid: 12345,
  command: 'claude -p test',
  name: 'test-session',
  project: 'synapse',
}

describe('AgentDetail', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockAgents.clear()
  })

  afterEach(() => {
    cleanup()
  })

  it('shows loading when agent not in cache', () => {
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    expect(screen.getByText('Loading...')).toBeDefined()
  })

  it('shows agent details when cached', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 })).toBeDefined()
    })
  })

  it('shows project name as title', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      const heading = screen.getByRole('heading', { level: 1 })
      expect(heading.textContent).toBe('synapse')
    })
  })

  it('shows state badge', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText(/running/)).toBeDefined()
    })
  })

  it('shows mode, external badge, taskId, cost, PID, session info', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('headless')).toBeDefined()
      expect(screen.getByText('external')).toBeDefined()
      expect(screen.getByText('task-1')).toBeDefined()
      expect(screen.getByText('$0.57')).toBeDefined()
      expect(screen.getByText('12345')).toBeDefined()
      expect(screen.getByText('sess-123')).toBeDefined()
      expect(screen.getByText('claude -p test')).toBeDefined()
    })
  })

  it('subscribes to EventsOn with correct channel', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalledWith(
        agentState('agent-1'),
        expect.any(Function),
      )
    })
  })

  it('shows back to agents button', () => {
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    expect(screen.getByText('Back to agents')).toBeDefined()
  })
})
