import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockGetRunLog = vi.fn()
const mockGetRunConvoLog = vi.fn()

vi.mock('$lib/api', () => ({
  GetAgentRunLog: (...args: unknown[]) => mockGetRunLog(...args),
  GetAgentRunConvoLog: (...args: unknown[]) => mockGetRunConvoLog(...args),
}))

vi.mock('../StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../MessageBubble.svelte', () => ({ default: () => {} }))
vi.mock('../ProviderLogo.svelte', () => ({ default: () => {} }))

const AgentHistoryList = (await import('./AgentHistoryList.svelte')).default

const headlessRun = {
  agentId: 'a-old-1',
  mode: 'headless',
  state: 'stopped',
  startedAt: '2026-04-01T00:00:00Z',
  costUsd: 0.0123,
  prompt: 'first prompt',
  result: '',
  logFile: '',
}
const interactiveRun = {
  agentId: 'a-old-2',
  mode: 'interactive',
  state: 'stopped',
  startedAt: '2026-04-02T00:00:00Z',
  costUsd: 0,
  prompt: '',
  result: '',
  logFile: '',
}

const baseTask = {
  id: 't1',
  title: 'X',
  status: 'done',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  agentRuns: [headlessRun, interactiveRun],
}

describe('AgentHistoryList', () => {
  beforeEach(() => {
    mockGetRunLog.mockReset()
    mockGetRunConvoLog.mockReset()
  })
  afterEach(cleanup)

  it('renders nothing when no past runs', () => {
    const { container } = render(AgentHistoryList, {
      props: { task: { ...baseTask, agentRuns: [] } as never },
    })
    expect(container.querySelector('button')).toBeNull()
  })

  it('lists past runs by id', () => {
    render(AgentHistoryList, { props: { task: baseTask as never } })
    expect(screen.getByText('a-old-1')).toBeDefined()
    expect(screen.getByText('a-old-2')).toBeDefined()
  })

  it('expanding headless run fetches stream log', async () => {
    mockGetRunLog.mockResolvedValue([{ type: 'assistant', content: 'hello' }])
    render(AgentHistoryList, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('a-old-1'))
    await waitFor(() => {
      expect(mockGetRunLog).toHaveBeenCalledWith('t1', 'a-old-1')
    })
  })

  it('expanding interactive run fetches convo log', async () => {
    mockGetRunConvoLog.mockResolvedValue([])
    render(AgentHistoryList, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('a-old-2'))
    await waitFor(() => {
      expect(mockGetRunConvoLog).toHaveBeenCalledWith('t1', 'a-old-2')
    })
  })

  it('shows a recoverable, human error with a retry when log fetch fails', async () => {
    mockGetRunLog.mockRejectedValue(new Error('boom'))
    render(AgentHistoryList, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('a-old-1'))
    await waitFor(() => {
      expect(screen.getByText(/Couldn't load this run's log/)).toBeDefined()
      expect(screen.getByText('Retry')).toBeDefined()
      // The raw error stays available under "Details", not as the headline.
      expect(screen.getByText(/boom/)).toBeDefined()
    })
  })

  it('Retry re-fetches the log and recovers', async () => {
    mockGetRunLog.mockRejectedValueOnce(new Error('boom')).mockResolvedValueOnce([])
    render(AgentHistoryList, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('a-old-1'))
    await waitFor(() => expect(screen.getByText('Retry')).toBeDefined())
    await fireEvent.click(screen.getByText('Retry'))
    await waitFor(() => {
      expect(mockGetRunLog).toHaveBeenCalledTimes(2)
      // Error cleared; the empty successful load falls through to "No output".
      expect(screen.queryByText(/Couldn't load this run's log/)).toBeNull()
    })
  })

  it('caches log on second open', async () => {
    mockGetRunLog.mockResolvedValue([])
    render(AgentHistoryList, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('a-old-1'))
    await waitFor(() => expect(mockGetRunLog).toHaveBeenCalledTimes(1))
    await fireEvent.click(screen.getByText('a-old-1')) // collapse
    await fireEvent.click(screen.getByText('a-old-1')) // re-open
    await waitFor(() => {
      // Still 1 call — cached
      expect(mockGetRunLog).toHaveBeenCalledTimes(1)
    })
  })
})
