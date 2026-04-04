import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { agentOutput } from '../lib/events.js'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockGetOutput = vi.fn()
const mockAppendEvent = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    getOutput: (...args: unknown[]) => mockGetOutput(...args),
    appendEvent: (...args: unknown[]) => mockAppendEvent(...args),
  },
}))

function makeEvent(type: string, content: string | undefined = undefined) {
  return { type, content }
}

describe('StreamOutput', () => {
  let StreamOutput: typeof import('./StreamOutput.svelte').default

  beforeEach(async () => {
    vi.clearAllMocks()
    mockGetOutput.mockResolvedValue([])
    StreamOutput = (await import('./StreamOutput.svelte')).default
  })

  afterEach(() => {
    cleanup()
  })

  it('shows waiting message when no events', async () => {
    render(StreamOutput, { props: { agentId: 'test-1' } })
    await vi.waitFor(() => {
      expect(screen.getByText('Waiting for output...')).toBeDefined()
    })
  })

  it('renders events when getOutput returns data', async () => {
    mockGetOutput.mockResolvedValue([
      makeEvent('assistant', 'Hello world'),
      makeEvent('tool_use', 'Running tests'),
    ])

    render(StreamOutput, { props: { agentId: 'test-1' } })

    await vi.waitFor(() => {
      expect(screen.getByText('ASST')).toBeDefined()
      expect(screen.getByText('Hello world')).toBeDefined()
      expect(screen.getByText('TOOL')).toBeDefined()
      expect(screen.getByText('Running tests')).toBeDefined()
    })
  })

  it('renders unknown event type label as uppercase', async () => {
    mockGetOutput.mockResolvedValue([makeEvent('custom', 'data')])

    render(StreamOutput, { props: { agentId: 'test-1' } })

    await vi.waitFor(() => {
      expect(screen.getByText('CUSTOM')).toBeDefined()
    })
  })

  it('renders all known type labels', async () => {
    mockGetOutput.mockResolvedValue([
      makeEvent('init', ''),
      makeEvent('assistant', ''),
      makeEvent('tool_use', ''),
      makeEvent('tool_result', ''),
      makeEvent('result', ''),
    ])

    render(StreamOutput, { props: { agentId: 'test-1' } })

    await vi.waitFor(() => {
      expect(screen.getByText('INIT')).toBeDefined()
      expect(screen.getByText('ASST')).toBeDefined()
      expect(screen.getByText('TOOL')).toBeDefined()
      expect(screen.getByText('RSLT')).toBeDefined()
      expect(screen.getByText('DONE')).toBeDefined()
    })
  })

  it('handles event with undefined content', async () => {
    mockGetOutput.mockResolvedValue([makeEvent('assistant')])

    render(StreamOutput, { props: { agentId: 'test-1' } })

    await vi.waitFor(() => {
      expect(screen.getByText('ASST')).toBeDefined()
    })
  })

  it('subscribes to EventsOn with correct channel name', async () => {
    render(StreamOutput, { props: { agentId: 'agent-42' } })

    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalledWith(agentOutput('agent-42'), expect.any(Function))
    })
  })

  it('calls getOutput with correct agentId', async () => {
    render(StreamOutput, { props: { agentId: 'agent-99' } })

    await vi.waitFor(() => {
      expect(mockGetOutput).toHaveBeenCalledWith('agent-99')
    })
  })

  it('appends event and updates UI when EventsOn callback fires', async () => {
    render(StreamOutput, { props: { agentId: 'test-1' } })

    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalled()
    })

    const callback = (mockEventsOn.mock.calls[0] as [string, (event: unknown) => void])[1]
    callback(makeEvent('result', 'Final output'))

    await vi.waitFor(() => {
      expect(screen.getByText('DONE')).toBeDefined()
      expect(screen.getByText('Final output')).toBeDefined()
      expect(mockAppendEvent).toHaveBeenCalledWith('test-1', makeEvent('result', 'Final output'))
    })
  })

  it('calls unsub on cleanup', async () => {
    const mockUnsub = vi.fn()
    mockEventsOn.mockReturnValue(mockUnsub)

    const { unmount } = render(StreamOutput, { props: { agentId: 'test-1' } })

    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalled()
    })

    unmount()

    await vi.waitFor(() => {
      expect(mockUnsub).toHaveBeenCalled()
    })
  })
})
