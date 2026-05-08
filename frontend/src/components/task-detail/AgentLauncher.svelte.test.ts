import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockStart = vi.fn()
const mockStop = vi.fn()
const mockByTask = vi.fn()
const mockUpdateAgent = vi.fn()
const mockEventsOn = vi.fn((..._args: unknown[]) => vi.fn())

let onlineState = true

vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: {
    start: (...args: unknown[]) => mockStart(...args),
    stop: (...args: unknown[]) => mockStop(...args),
    byTask: (...args: unknown[]) => mockByTask(...args),
    updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
  },
}))

vi.mock('../../stores/connection.svelte.js', () => ({
  connectionStore: {
    get online() {
      return onlineState
    },
  },
}))

vi.mock('$lib/api', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

vi.mock('../StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../ChatView.svelte', () => ({ default: () => {} }))
vi.mock('../ProviderLogo.svelte', () => ({ default: () => {} }))

const AgentLauncher = (await import('./AgentLauncher.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'X',
  status: 'todo',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('AgentLauncher', () => {
  beforeEach(() => {
    mockStart.mockReset()
    mockStop.mockReset()
    mockByTask.mockReset()
    mockUpdateAgent.mockReset()
    mockEventsOn.mockReset()
    mockEventsOn.mockReturnValue(vi.fn())
    mockByTask.mockReturnValue(undefined)
    onlineState = true
  })
  afterEach(cleanup)

  it('shows Start agent form when no running agent', () => {
    render(AgentLauncher, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    expect(screen.getByText('Start agent')).toBeDefined()
  })

  it('Start agent button is disabled when prompt empty', () => {
    render(AgentLauncher, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    const btn = screen.getByText('Start agent') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
  })

  it('calls agentStore.start with prompt', async () => {
    mockStart.mockResolvedValue({ id: 'a1', state: 'running', mode: 'headless' })
    render(AgentLauncher, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    const ta = screen.getByPlaceholderText('Enter prompt for the agent...') as HTMLTextAreaElement
    await fireEvent.input(ta, { target: { value: 'do the thing' } })
    await fireEvent.click(screen.getByText('Start agent'))
    await waitFor(() => {
      expect(mockStart).toHaveBeenCalledWith('t1', 'headless', 'do the thing')
    })
  })

  it('shows Offline indicator when connection.offline', () => {
    onlineState = false
    render(AgentLauncher, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    expect(screen.getByText('Offline')).toBeDefined()
  })

  it('renders running agent header with stop button', () => {
    mockByTask.mockReturnValue({
      id: 'a1',
      state: 'running',
      mode: 'headless',
      prompt: 'hi',
      taskId: 't1',
    })
    render(AgentLauncher, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    expect(screen.getByText('Stop')).toBeDefined()
  })

  it('clicking Stop calls agentStore.stop', async () => {
    mockByTask.mockReturnValue({
      id: 'a1',
      state: 'running',
      mode: 'headless',
      taskId: 't1',
    })
    render(AgentLauncher, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    await fireEvent.click(screen.getByText('Stop'))
    expect(mockStop).toHaveBeenCalledWith('a1')
  })
})
