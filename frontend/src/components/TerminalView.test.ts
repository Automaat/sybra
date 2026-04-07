import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import { agent } from '../../wailsjs/go/models.js'

const mockCaptureAgentPane = vi.fn()
const mockAttachAgent = vi.fn()

vi.mock('../../wailsjs/go/main/AgentService.js', () => ({
  CaptureAgentPane: (...args: unknown[]) => mockCaptureAgentPane(...args),
  AttachAgent: (...args: unknown[]) => mockAttachAgent(...args),
  ListAgents: vi.fn().mockResolvedValue([]),
  StopAgent: vi.fn(),
  GetAgentOutput: vi.fn().mockResolvedValue([]),
  DiscoverAgents: vi.fn().mockResolvedValue([]),
}))

vi.mock('../../wailsjs/go/main/App.js', () => ({
  StartAgent: vi.fn(),
}))

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: vi.fn().mockReturnValue(() => {}),
  EventsOff: vi.fn(),
  EventsEmit: vi.fn(),
}))

const { agentStore } = await import('../stores/agents.svelte.js')

import TerminalView from './TerminalView.svelte'

function makeAgent(overrides: Partial<agent.Agent> = {}): agent.Agent {
  return agent.Agent.createFrom({
    id: 'agent-1',
    taskId: 'task-1',
    mode: 'interactive',
    state: 'running',
    sessionId: '',
    tmuxSession: 'synapse-agent-1',
    costUsd: 0,
    startedAt: new Date().toISOString(),
    external: false,
    ...overrides,
  })
}

describe('TerminalView', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    agentStore.agents = new Map()
    agentStore.outputs = new Map()
    agentStore.error = ''
    agentStore.loading = false
  })

  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it('renders waiting message when no output', async () => {
    mockCaptureAgentPane.mockResolvedValue('')
    agentStore.agents.set('agent-1', makeAgent())

    render(TerminalView, { props: { agentId: 'agent-1' } })

    await vi.advanceTimersByTimeAsync(0)
    expect(screen.getByText('Waiting for output...')).toBeTruthy()
  })

  it('displays captured pane output', async () => {
    mockCaptureAgentPane.mockResolvedValue('Hello world')
    agentStore.agents.set('agent-1', makeAgent())

    render(TerminalView, { props: { agentId: 'agent-1' } })

    await vi.advanceTimersByTimeAsync(0)
    expect(screen.getByText('Hello world')).toBeTruthy()
  })

  it('shows attach button when agent is running', async () => {
    mockCaptureAgentPane.mockResolvedValue('output')
    agentStore.agents.set('agent-1', makeAgent({ state: 'running' }))

    render(TerminalView, { props: { agentId: 'agent-1' } })

    await vi.advanceTimersByTimeAsync(0)
    expect(screen.getByText('Attach in Terminal')).toBeTruthy()
  })

  it('hides attach button when agent is stopped', async () => {
    mockCaptureAgentPane.mockResolvedValue('output')
    agentStore.agents.set('agent-1', makeAgent({ state: 'stopped' }))

    render(TerminalView, { props: { agentId: 'agent-1' } })

    await vi.advanceTimersByTimeAsync(0)
    expect(screen.queryByText('Attach in Terminal')).toBeNull()
  })

  it('polls capture-pane on interval', async () => {
    mockCaptureAgentPane.mockResolvedValue('line1')
    agentStore.agents.set('agent-1', makeAgent())

    render(TerminalView, { props: { agentId: 'agent-1' } })

    await vi.advanceTimersByTimeAsync(0)
    expect(mockCaptureAgentPane).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(1000)
    expect(mockCaptureAgentPane).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(1000)
    expect(mockCaptureAgentPane).toHaveBeenCalledTimes(3)
  })

  it('shows error on capture failure', async () => {
    mockCaptureAgentPane.mockRejectedValue(new Error('tmux not running'))
    agentStore.agents.set('agent-1', makeAgent())

    render(TerminalView, { props: { agentId: 'agent-1' } })

    await vi.advanceTimersByTimeAsync(0)
    expect(screen.getByText('Error: tmux not running')).toBeTruthy()
  })

  it('shows label text', async () => {
    mockCaptureAgentPane.mockResolvedValue('')
    agentStore.agents.set('agent-1', makeAgent())

    render(TerminalView, { props: { agentId: 'agent-1' } })

    expect(screen.getByText('tmux capture-pane (live)')).toBeTruthy()
  })
})
