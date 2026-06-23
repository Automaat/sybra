import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import {
  agentState,
  agentError,
  agentEscalation,
  agentPluginErrors,
} from '../lib/events.js'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { taskStore } from '../stores/tasks.svelte.js'

const mockTasks = taskStore.tasks as Map<string, unknown>

const mockStop = vi.fn()
const mockUpdateAgent = vi.fn()
const mockRespondEscalation = vi.fn()

type Handler = (data: unknown) => void
const eventHandlers: Record<string, Handler> = {}

const mockEventsOn = vi.fn((channel: string, cb: Handler) => {
  eventHandlers[channel] = cb
  return vi.fn()
})

const mockAgents = new Map()

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    agents: mockAgents,
    list: [] as unknown[],
    outputs: new Map<string, unknown[]>(),
    stepTexts: new Map<string, string>(),
    stop: (...args: unknown[]) => mockStop(...args),
    updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
  },
}))

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    tasks: new Map<string, unknown>(),
  },
}))

vi.mock('../stores/convo.svelte.js', () => ({
  convoStore: {
    conversations: new Map<string, unknown[]>(),
  },
}))

vi.mock('$lib/api', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...(args as [string, Handler])),
  RespondEscalation: (...args: unknown[]) => mockRespondEscalation(...args),
}))

vi.mock('../components/StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../components/agent-view/SessionWorkspace.svelte', () => ({ default: () => {} }))
vi.mock('../components/agent-view/AgentViewBody.svelte', () => ({ default: () => {} }))

const AgentDetail = (await import('./AgentDetail.svelte')).default

const mockAgent = {
  id: 'agent-1',
  taskId: 'task-1',
  mode: 'headless',
  state: 'running',
  sessionId: 'sess-123',
  costUsd: 0.5678,
  startedAt: '2026-04-01T00:00:00Z',
  external: true,
  pid: 12345,
  command: 'claude -p test',
  name: 'test-session',
  project: 'sybra',
}

describe('AgentDetail', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockAgents.clear()
    mockTasks.clear()
    for (const k of Object.keys(eventHandlers)) delete eventHandlers[k]
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

  it('shows the session name as the title', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      const heading = screen.getByRole('heading', { level: 1 })
      expect(heading.textContent).toBe('test-session')
    })
  })

  it('shows the project as the title when there is no name', async () => {
    mockAgents.set('agent-1', { ...mockAgent, name: '' })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      const heading = screen.getByRole('heading', { level: 1 })
      expect(heading.textContent).toBe('sybra')
    })
  })

  it('shows a labelled session id when name, project and task are empty', async () => {
    mockAgents.set('agent-1', { ...mockAgent, name: '', project: '', taskId: '' })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      const heading = screen.getByRole('heading', { level: 1 })
      expect(heading.textContent).toBe('Session agent-1')
    })
  })

  it('shows the task name once for a role-prefixed agent with a linked task', async () => {
    // Review agent: name is `review:<task title>`, task linked. The heading is
    // the task title; the role-stripped subtitle must not repeat it.
    mockTasks.set('task-1', { id: 'task-1', title: 'Fix the auth bug' })
    mockAgents.set('agent-1', { ...mockAgent, name: 'review:Fix the auth bug' })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Fix the auth bug')
    })
    // Appears exactly once — no duplicate subtitle, no `review:` prefix anywhere.
    expect(screen.getAllByText('Fix the auth bug')).toHaveLength(1)
    expect(screen.queryByText('review:Fix the auth bug')).toBeNull()
  })

  it('shows Stop button when agent is running', async () => {
    mockAgents.set('agent-1', { ...mockAgent, state: 'running' })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Stop')).toBeDefined()
    })
  })

  it('does not show Stop button when agent is stopped', async () => {
    mockAgents.set('agent-1', { ...mockAgent, state: 'stopped' })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.queryByText('Stop')).toBeNull()
    })
  })

  it('calls agentStore.stop when Stop button clicked', async () => {
    mockAgents.set('agent-1', { ...mockAgent, state: 'running' })
    mockStop.mockResolvedValue(undefined)
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Stop'))
    await fireEvent.click(screen.getByText('Stop'))
    await vi.waitFor(() => {
      expect(mockStop).toHaveBeenCalledWith('agent-1')
    })
  })

  it('shows error text when Stop call rejects', async () => {
    mockAgents.set('agent-1', { ...mockAgent, state: 'running' })
    mockStop.mockRejectedValue(new Error('stop failed'))
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => screen.getByText('Stop'))
    await fireEvent.click(screen.getByText('Stop'))
    await vi.waitFor(() => {
      expect(screen.getByText(/stop failed/)).toBeDefined()
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
      expect(screen.getByText('View task →')).toBeDefined()
      expect(screen.getByText('$0.57')).toBeDefined()
      expect(screen.getByText('12345')).toBeDefined()
      expect(screen.getByText('sess-123')).toBeDefined()
      expect(screen.getByText('claude -p test')).toBeDefined()
    })
  })

  it('subscribes to all four agent event channels', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalledWith(agentState('agent-1'), expect.any(Function))
      expect(mockEventsOn).toHaveBeenCalledWith(agentError('agent-1'), expect.any(Function))
      expect(mockEventsOn).toHaveBeenCalledWith(agentEscalation('agent-1'), expect.any(Function))
      expect(mockEventsOn).toHaveBeenCalledWith(agentPluginErrors('agent-1'), expect.any(Function))
    })
  })

  it('shows turns escalation banner when agentEscalation fires with turns reason', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentEscalation('agent-1')])
    eventHandlers[agentEscalation('agent-1')]({
      reason: 'turns',
      turnCount: 50,
      limit: 50,
    })
    await vi.waitFor(() => {
      expect(screen.getByText('GUARDRAIL')).toBeDefined()
      expect(screen.getByText(/Turn limit reached/)).toBeDefined()
      expect(screen.getByText('Continue')).toBeDefined()
      expect(screen.getByText('Kill')).toBeDefined()
    })
  })

  it('shows cost escalation banner when agentEscalation fires with cost reason', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentEscalation('agent-1')])
    eventHandlers[agentEscalation('agent-1')]({
      reason: 'cost',
      costUsd: 5.5,
      limit: 5,
    })
    await vi.waitFor(() => {
      expect(screen.getByText('GUARDRAIL')).toBeDefined()
      expect(screen.getByText(/Cost limit exceeded/)).toBeDefined()
      expect(screen.queryByText('Continue')).toBeNull()
      expect(screen.getByText('Dismiss')).toBeDefined()
    })
  })

  it('calls RespondEscalation(true) on Continue click and clears banner', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    mockRespondEscalation.mockResolvedValue(undefined)
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentEscalation('agent-1')])
    eventHandlers[agentEscalation('agent-1')]({
      reason: 'turns',
      turnCount: 50,
      limit: 50,
    })
    await vi.waitFor(() => screen.getByText('Continue'))
    await fireEvent.click(screen.getByText('Continue'))
    await vi.waitFor(() => {
      expect(mockRespondEscalation).toHaveBeenCalledWith('agent-1', true)
      expect(screen.queryByText('GUARDRAIL')).toBeNull()
    })
  })

  it('calls RespondEscalation(false) on Kill click and clears banner', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    mockRespondEscalation.mockResolvedValue(undefined)
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentEscalation('agent-1')])
    eventHandlers[agentEscalation('agent-1')]({
      reason: 'turns',
      turnCount: 50,
      limit: 50,
    })
    await vi.waitFor(() => screen.getByText('Kill'))
    await fireEvent.click(screen.getByText('Kill'))
    await vi.waitFor(() => {
      expect(mockRespondEscalation).toHaveBeenCalledWith('agent-1', false)
      expect(screen.queryByText('GUARDRAIL')).toBeNull()
    })
  })

  it('dismisses cost escalation even when RespondEscalation rejects', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    mockRespondEscalation.mockRejectedValue(new Error('already stopped'))
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentEscalation('agent-1')])
    eventHandlers[agentEscalation('agent-1')]({
      reason: 'cost',
      costUsd: 5.5,
      limit: 5,
    })
    await vi.waitFor(() => screen.getByText('Dismiss'))
    await fireEvent.click(screen.getByText('Dismiss'))
    await vi.waitFor(() => {
      expect(screen.queryByText('GUARDRAIL')).toBeNull()
    })
  })

  it('shows plugin errors banner when agentPluginErrors fires', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentPluginErrors('agent-1')])
    eventHandlers[agentPluginErrors('agent-1')]({
      errors: ['plugin foo: failed to load', 'plugin bar: missing dep'],
    })
    await vi.waitFor(() => {
      expect(screen.getByText('PLUGIN ERRORS')).toBeDefined()
      expect(screen.getByText('2 plugins failed to load')).toBeDefined()
      expect(screen.getByText('plugin foo: failed to load')).toBeDefined()
      expect(screen.getByText('plugin bar: missing dep')).toBeDefined()
    })
  })

  it('uses singular plugin label when only one plugin failed', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentPluginErrors('agent-1')])
    eventHandlers[agentPluginErrors('agent-1')]({
      errors: ['plugin foo: failed'],
    })
    await vi.waitFor(() => {
      expect(screen.getByText('1 plugin failed to load')).toBeDefined()
    })
  })

  it('hides plugin errors banner when Dismiss is clicked', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentPluginErrors('agent-1')])
    eventHandlers[agentPluginErrors('agent-1')]({
      errors: ['plugin foo: failed'],
    })
    await vi.waitFor(() => screen.getByText('Dismiss'))
    await fireEvent.click(screen.getByText('Dismiss'))
    await vi.waitFor(() => {
      expect(screen.queryByText('PLUGIN ERRORS')).toBeNull()
    })
  })

  it('seeds plugin errors from cached agent on mount', async () => {
    mockAgents.set('agent-1', { ...mockAgent, pluginErrors: ['cached plugin err'] })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('PLUGIN ERRORS')).toBeDefined()
      expect(screen.getByText('cached plugin err')).toBeDefined()
    })
  })

  it('shows error banner when agentError event fires', async () => {
    mockAgents.set('agent-1', { ...mockAgent })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentError('agent-1')])
    eventHandlers[agentError('agent-1')]({
      kind: 'rate_limit',
      msg: 'API rate limit hit',
    })
    await vi.waitFor(() => {
      expect(screen.getByText('API rate limited')).toBeDefined()
    })
  })

  it('seeds error banner from cached agent errorKind', async () => {
    mockAgents.set('agent-1', {
      ...mockAgent,
      errorKind: 'worktree_conflict',
      errorMsg: 'already checked out',
    })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Worktree conflict')).toBeDefined()
    })
  })

  it('updates state when agentState event fires', async () => {
    mockAgents.set('agent-1', { ...mockAgent, state: 'running' })
    mockUpdateAgent.mockImplementation((id: string, data: unknown) => {
      mockAgents.set(id, data)
    })
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    await vi.waitFor(() => eventHandlers[agentState('agent-1')])
    eventHandlers[agentState('agent-1')]({ ...mockAgent, state: 'stopped' })
    await vi.waitFor(() => {
      expect(mockUpdateAgent).toHaveBeenCalledWith('agent-1', expect.objectContaining({ state: 'stopped' }))
    })
  })

  it('shows back to agents button', () => {
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() },
    })
    expect(screen.getByText('Back to agents')).toBeDefined()
  })

  it('calls onback when back button clicked', async () => {
    const onback = vi.fn()
    render(AgentDetail, {
      props: { agentId: 'agent-1', onback, onviewtask: vi.fn() },
    })
    await fireEvent.click(screen.getByText('Back to agents'))
    expect(onback).toHaveBeenCalled()
  })
})
