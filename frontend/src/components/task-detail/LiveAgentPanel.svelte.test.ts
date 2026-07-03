import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockStop = vi.fn()
const mockByTask = vi.fn()
const mockUpdateAgent = vi.fn()
const mockEventsOn = vi.fn((..._args: unknown[]) => vi.fn())

vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: {
    stop: (...args: unknown[]) => mockStop(...args),
    byTask: (...args: unknown[]) => mockByTask(...args),
    updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
  },
}))

vi.mock('$lib/api', () => ({
  EventsOn: (...args: unknown[]) => mockEventsOn(...args),
}))

vi.mock('../StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../ChatView.svelte', () => ({ default: () => {} }))
vi.mock('../ProviderLogo.svelte', () => ({ default: () => {} }))

const LiveAgentPanel = (await import('./LiveAgentPanel.svelte')).default

const baseTask = {
  id: 't1',
  title: 'X',
  status: 'in-progress',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('LiveAgentPanel', () => {
  beforeEach(() => {
    mockStop.mockReset()
    mockByTask.mockReset()
    mockUpdateAgent.mockReset()
    mockEventsOn.mockReset()
    mockEventsOn.mockReturnValue(vi.fn())
    mockByTask.mockReturnValue(undefined)
  })
  afterEach(cleanup)

  it('renders nothing when no running agent', () => {
    const { container } = render(LiveAgentPanel, {
      props: { task: baseTask as never, onviewagent: vi.fn() },
    })
    expect(container.textContent).toBe('')
  })

  it('renders the running agent header with a Stop button', () => {
    mockByTask.mockReturnValue({ id: 'a1', state: 'running', mode: 'headless', prompt: 'hi', taskId: 't1' })
    render(LiveAgentPanel, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    expect(screen.getByText('a1')).toBeDefined()
    expect(screen.getByText('Stop')).toBeDefined()
  })

  it('clicking Stop calls agentStore.stop', async () => {
    mockByTask.mockReturnValue({ id: 'a1', state: 'running', mode: 'headless', taskId: 't1' })
    render(LiveAgentPanel, { props: { task: baseTask as never, onviewagent: vi.fn() } })
    await fireEvent.click(screen.getByText('Stop'))
    expect(mockStop).toHaveBeenCalledWith('a1')
  })

  it('calls onviewagent when the agent id is clicked', async () => {
    mockByTask.mockReturnValue({ id: 'a1', state: 'running', mode: 'headless', taskId: 't1' })
    const onviewagent = vi.fn()
    render(LiveAgentPanel, { props: { task: baseTask as never, onviewagent } })
    await fireEvent.click(screen.getByText('a1'))
    expect(onviewagent).toHaveBeenCalledWith('a1')
  })

  it('ignores a non-running agent for the task', () => {
    mockByTask.mockReturnValue({ id: 'a1', state: 'stopped', mode: 'headless', taskId: 't1' })
    const { container } = render(LiveAgentPanel, {
      props: { task: baseTask as never, onviewagent: vi.fn() },
    })
    expect(container.textContent).toBe('')
  })

  // A plan-review task can keep an interactive planner running; its live view
  // must still show (it lives above the tabs, independent of task status).
  it('shows a running agent even on a plan-review task', () => {
    mockByTask.mockReturnValue({ id: 'a1', state: 'running', mode: 'interactive', taskId: 't1' })
    render(LiveAgentPanel, {
      props: { task: { ...baseTask, status: 'plan-review' } as never, onviewagent: vi.fn() },
    })
    expect(screen.getByText('Stop')).toBeDefined()
  })
})
