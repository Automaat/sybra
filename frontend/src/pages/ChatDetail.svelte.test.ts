import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())
const mockStop = vi.fn()
const mockStopChat = vi.fn()
const mockUpdateAgent = vi.fn()

const agentMap = new Map<string, any>()

vi.mock('$lib/api', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    get agents() { return agentMap },
    stop: (...args: unknown[]) => mockStop(...args),
    stopChat: (...args: unknown[]) => mockStopChat(...args),
    updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
  },
}))

vi.mock('../components/ChatView.svelte', () => ({ default: () => {} }))

const ChatDetail = (await import('./ChatDetail.svelte')).default

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'agent-1',
    name: 'my-chat',
    taskId: 'task-1',
    state: 'running',
    costUsd: 0,
    project: '',
    mode: 'interactive',
    inputTokens: 0,
    outputTokens: 0,
    awaitingApproval: false,
    ...overrides,
  }
}

describe('ChatDetail', () => {
  beforeEach(() => {
    agentMap.clear()
    mockEventsOn.mockReturnValue(vi.fn())
    mockStop.mockReset()
    mockStopChat.mockReset()
    mockUpdateAgent.mockReset()
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('renders back button linking to Chats', () => {
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('Chats')).toBeDefined()
  })

  it('calls onback when back button clicked', async () => {
    const onback = vi.fn()
    render(ChatDetail, { props: { agentId: 'agent-1', onback, onviewtask: vi.fn() } })
    await fireEvent.click(screen.getByText('Chats'))
    expect(onback).toHaveBeenCalled()
  })

  it('shows Loading... when agent is not in store', () => {
    render(ChatDetail, { props: { agentId: 'agent-99', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('Loading...')).toBeDefined()
  })

  it('renders agent name from store', () => {
    agentMap.set('agent-1', makeAgent({ name: 'fix-auth' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('fix-auth')).toBeDefined()
  })

  it('shows task id as link when agent has taskId', () => {
    agentMap.set('agent-1', makeAgent({ taskId: 'task-abc' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('task-abc')).toBeDefined()
  })

  it('calls onviewtask when task id link clicked', async () => {
    const onviewtask = vi.fn()
    agentMap.set('agent-1', makeAgent({ taskId: 'task-xyz' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask } })
    await fireEvent.click(screen.getByText('task-xyz'))
    expect(onviewtask).toHaveBeenCalledWith('task-xyz')
  })

  it('shows project badge when agent has project', () => {
    agentMap.set('agent-1', makeAgent({ project: 'org/repo' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('org/repo')).toBeDefined()
  })

  it('shows Stop button when agent is running', () => {
    agentMap.set('agent-1', makeAgent({ state: 'running' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('Stop')).toBeDefined()
  })

  it('shows End chat button when agent exists', () => {
    agentMap.set('agent-1', makeAgent())
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.getByText('End chat')).toBeDefined()
  })

  it('does not show Stop button when agent is done', () => {
    agentMap.set('agent-1', makeAgent({ state: 'done' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(screen.queryByText('Stop')).toBeNull()
  })

  it('calls agentStore.stop when Stop clicked', async () => {
    mockStop.mockResolvedValue(undefined)
    agentMap.set('agent-1', makeAgent({ state: 'running' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    await fireEvent.click(screen.getByText('Stop'))
    expect(mockStop).toHaveBeenCalledWith('agent-1')
  })

  it('calls agentStore.stopChat and onback when End chat clicked', async () => {
    mockStopChat.mockResolvedValue(undefined)
    const onback = vi.fn()
    agentMap.set('agent-1', makeAgent())
    render(ChatDetail, { props: { agentId: 'agent-1', onback, onviewtask: vi.fn() } })
    await fireEvent.click(screen.getByText('End chat'))
    await vi.waitFor(() => {
      expect(mockStopChat).toHaveBeenCalledWith('agent-1')
      expect(onback).toHaveBeenCalled()
    })
  })

  it('shows error when Stop fails', async () => {
    mockStop.mockRejectedValue(new Error('network error'))
    agentMap.set('agent-1', makeAgent({ state: 'running' }))
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    await fireEvent.click(screen.getByText('Stop'))
    await vi.waitFor(() => {
      expect(screen.getByText('Error: network error')).toBeDefined()
    })
  })

  it('subscribes to agent state events', () => {
    render(ChatDetail, { props: { agentId: 'agent-1', onback: vi.fn(), onviewtask: vi.fn() } })
    expect(mockEventsOn).toHaveBeenCalled()
  })
})
