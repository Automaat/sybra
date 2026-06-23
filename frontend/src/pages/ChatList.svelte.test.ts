import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockAgentList: any[] = []
const mockProjectLoad = vi.fn()

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    get list() { return mockAgentList },
    get agents() { return new Map() },
  },
}))

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    list: [],
    load: (...args: unknown[]) => mockProjectLoad(...args),
  },
}))

vi.mock('../components/NewChatDialog.svelte', () => ({ default: () => {} }))

vi.mock('$lib/agent-phases.js', () => ({
  getAgentPhase: vi.fn(() => 'active'),
  PHASE_CONFIG: {
    active: { label: 'Running', dotClasses: 'bg-green-500', animate: true },
    done: { label: 'Done', dotClasses: 'bg-gray-500', animate: false },
    blocked: { label: 'Blocked', dotClasses: 'bg-red-500', animate: false },
  },
}))

const ChatList = (await import('./ChatList.svelte')).default

function makeAgent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'agent-1',
    name: 'chat-agent',
    taskId: 'task-1',
    mode: 'interactive',
    state: 'running',
    costUsd: 0,
    startedAt: new Date().toISOString(),
    project: '',
    awaitingApproval: false,
    escalationReason: '',
    inputTokens: 0,
    outputTokens: 0,
    ...overrides,
  }
}

describe('ChatList', () => {
  beforeEach(() => {
    mockAgentList.length = 0
    mockProjectLoad.mockResolvedValue(undefined)
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('renders the new-chat action', () => {
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('+ New Chat')).toBeDefined()
  })

  it('shows + New Chat button', () => {
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('+ New Chat')).toBeDefined()
  })

  it('shows empty state when no interactive agents', () => {
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('No interactive chats yet')).toBeDefined()
  })

  it('shows Start a new chat button in empty state', () => {
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Start a new chat')).toBeDefined()
  })

  it('renders agent list when interactive agents exist', () => {
    mockAgentList.push(makeAgent({ name: 'my-chat-session' }))
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('my-chat-session')).toBeDefined()
  })

  it('calls onselect with agent id when row clicked', async () => {
    const onselect = vi.fn()
    mockAgentList.push(makeAgent({ id: 'agent-42', name: 'click-me' }))
    render(ChatList, { props: { onselect } })
    await fireEvent.click(screen.getByText('click-me'))
    expect(onselect).toHaveBeenCalledWith('agent-42')
  })

  it('shows agent cost when costUsd > 0', () => {
    mockAgentList.push(makeAgent({ costUsd: 0.07 }))
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('$0.07')).toBeDefined()
  })

  it('shows project badge when agent has project', () => {
    mockAgentList.push(makeAgent({ project: 'org/myrepo' }))
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('org/myrepo')).toBeDefined()
  })

  it('shows a friendly session label instead of a bare id', () => {
    mockAgentList.push(makeAgent({ name: '', taskId: '', project: '', id: 'ext-codex-abcdef12' }))
    render(ChatList, { props: { onselect: vi.fn() } })
    // Trailing entropy kept so distinct ext-codex- sessions don't collide.
    expect(screen.getByText('Session abcdef12')).toBeDefined()
  })

  it('only shows interactive agents (not headless)', () => {
    mockAgentList.push(makeAgent({ id: 'i1', name: 'interactive', mode: 'interactive' }))
    mockAgentList.push(makeAgent({ id: 'h1', name: 'headless-agent', mode: 'headless' }))
    render(ChatList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('interactive')).toBeDefined()
    expect(screen.queryByText('headless-agent')).toBeNull()
  })
})
