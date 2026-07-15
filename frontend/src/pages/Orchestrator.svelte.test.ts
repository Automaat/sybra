import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockIsOrchestratorRunning = vi.fn().mockResolvedValue(false)
const mockStartOrchestrator = vi.fn().mockResolvedValue(undefined)
const mockStopOrchestrator = vi.fn().mockResolvedValue(undefined)
const mockGetOrchestratorAgentID = vi.fn().mockResolvedValue('')
const mockGetMonitorReport = vi.fn()

const mockGetOutput = vi.fn().mockResolvedValue([])
const mockSubscribe = vi.fn().mockReturnValue(() => {})

type Handler = (data: unknown) => void
const eventHandlers: Record<string, Handler> = {}

const mockEventsOn = vi.fn((channel: string, cb: Handler) => {
  eventHandlers[channel] = cb
  return vi.fn()
})

const mockAgentList: any[] = []
const mockConversations = new Map<string, unknown[]>()

const emptyMonitorBinding = () => ({
  enabled: true,
  ready: false,
  report: {
    generatedAt: '',
    counts: {
      new: 0,
      todo: 0,
      inProgress: 0,
      inReview: 0,
      planReview: 0,
      humanRequired: 0,
      done: 0,
      byStatus: {},
    },
    anomalies: [],
    remediated: [],
    dispatched: [],
    issuesOpened: 0,
    issuesUpdated: 0,
  },
})

vi.mock('$lib/api', () => ({
  IsOrchestratorRunning: (...args: unknown[]) => mockIsOrchestratorRunning(...args),
  StartOrchestrator: (...args: unknown[]) => mockStartOrchestrator(...args),
  StopOrchestrator: (...args: unknown[]) => mockStopOrchestrator(...args),
  GetOrchestratorAgentID: (...args: unknown[]) => mockGetOrchestratorAgentID(...args),
  GetMonitorReport: (...args: unknown[]) => mockGetMonitorReport(...args),
  EventsOn: (...args: any[]) => mockEventsOn(...(args as [string, Handler])),
  BrowserOpenURL: vi.fn(),
}))

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    get list() {
      return mockAgentList
    },
  },
}))

vi.mock('../stores/convo.svelte.js', () => ({
  convoStore: {
    conversations: mockConversations,
    getOutput: (...args: unknown[]) => mockGetOutput(...args),
    subscribe: (...args: unknown[]) => mockSubscribe(...args),
  },
}))

vi.mock('../components/StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../components/MessageBubble.svelte', () => ({ default: () => {} }))

const Orchestrator = (await import('./Orchestrator.svelte')).default

describe('Orchestrator', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockIsOrchestratorRunning.mockResolvedValue(false)
    mockGetOrchestratorAgentID.mockResolvedValue('')
    mockGetMonitorReport.mockResolvedValue(emptyMonitorBinding())
    mockEventsOn.mockImplementation((channel: string, cb: Handler) => {
      eventHandlers[channel] = cb
      return vi.fn()
    })
    mockGetOutput.mockResolvedValue([])
    mockSubscribe.mockReturnValue(() => {})
    mockAgentList.length = 0
    mockConversations.clear()
    for (const k of Object.keys(eventHandlers)) delete eventHandlers[k]
  })

  afterEach(() => {
    cleanup()
  })

  it('renders Interactive Session heading', () => {
    render(Orchestrator, { props: {} })
    expect(screen.getByText('Interactive Session')).toBeDefined()
  })

  it('shows Stopped status initially', async () => {
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Stopped')).toBeDefined()
    })
  })

  it('renders Triage Agents section', () => {
    render(Orchestrator, { props: {} })
    expect(screen.getByText('Triage Agents')).toBeDefined()
  })

  it('renders Eval Agents section', () => {
    render(Orchestrator, { props: {} })
    expect(screen.getByText('Eval Agents')).toBeDefined()
  })

  it('shows empty triage message when no triage agents', () => {
    render(Orchestrator, { props: {} })
    expect(screen.getByText('No triage sessions yet. Create a task to trigger auto-triage.')).toBeDefined()
  })

  it('shows empty eval message when no eval agents', () => {
    render(Orchestrator, { props: {} })
    expect(screen.getByText('No evaluations yet. Agents trigger eval on completion.')).toBeDefined()
  })

  it('shows Start button when not running', async () => {
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Start')).toBeDefined()
    })
  })

  it('subscribes to OrchestratorState event', async () => {
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalledWith('orchestrator:state', expect.any(Function))
    })
  })

  it('subscribes to MonitorReport event', async () => {
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalledWith('monitor:report', expect.any(Function))
    })
  })

  it('shows Running status when orchestrator already running on mount', async () => {
    mockIsOrchestratorRunning.mockResolvedValue(true)
    mockGetOrchestratorAgentID.mockResolvedValue('orch-1')
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Running')).toBeDefined()
      expect(screen.getByText('Stop')).toBeDefined()
    })
  })

  it('calls StartOrchestrator when Start clicked', async () => {
    mockStartOrchestrator.mockResolvedValue(undefined)
    mockGetOrchestratorAgentID.mockResolvedValue('orch-1')
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => screen.getByText('Start'))
    await fireEvent.click(screen.getByText('Start'))
    await vi.waitFor(() => {
      expect(mockStartOrchestrator).toHaveBeenCalled()
      expect(screen.getByText('Running')).toBeDefined()
    })
  })

  it('shows error when Start fails', async () => {
    mockStartOrchestrator.mockRejectedValue(new Error('start failed'))
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => screen.getByText('Start'))
    await fireEvent.click(screen.getByText('Start'))
    await vi.waitFor(() => {
      expect(screen.getByText(/start failed/)).toBeDefined()
    })
  })

  it('calls StopOrchestrator when Stop clicked', async () => {
    mockIsOrchestratorRunning.mockResolvedValue(true)
    mockGetOrchestratorAgentID.mockResolvedValue('orch-1')
    mockStopOrchestrator.mockResolvedValue(undefined)
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => screen.getByText('Stop'))
    await fireEvent.click(screen.getByText('Stop'))
    await vi.waitFor(() => {
      expect(mockStopOrchestrator).toHaveBeenCalled()
      expect(screen.getByText('Stopped')).toBeDefined()
    })
  })

  it('shows error when Stop fails', async () => {
    mockIsOrchestratorRunning.mockResolvedValue(true)
    mockGetOrchestratorAgentID.mockResolvedValue('orch-1')
    mockStopOrchestrator.mockRejectedValue(new Error('stop failed'))
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => screen.getByText('Stop'))
    await fireEvent.click(screen.getByText('Stop'))
    await vi.waitFor(() => {
      expect(screen.getByText(/stop failed/)).toBeDefined()
    })
  })

  it('toggles to Running when OrchestratorState event fires with running', async () => {
    mockGetOrchestratorAgentID.mockResolvedValue('orch-1')
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => eventHandlers['orchestrator:state'])
    await eventHandlers['orchestrator:state']('running')
    await vi.waitFor(() => {
      expect(screen.getByText('Running')).toBeDefined()
    })
  })

  it('toggles to Stopped when OrchestratorState event fires with stopped', async () => {
    mockIsOrchestratorRunning.mockResolvedValue(true)
    mockGetOrchestratorAgentID.mockResolvedValue('orch-1')
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => eventHandlers['orchestrator:state'])
    await eventHandlers['orchestrator:state']('stopped')
    await vi.waitFor(() => {
      expect(screen.getByText('Stopped')).toBeDefined()
    })
  })

  it('shows monitor: waiting when monitor enabled but not ready', async () => {
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('monitor: waiting')).toBeDefined()
    })
  })

  it('does not show monitor status when monitor disabled', async () => {
    mockGetMonitorReport.mockResolvedValue({ ...emptyMonitorBinding(), enabled: false })
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText('Stopped')).toBeDefined()
      expect(screen.queryByText('monitor: waiting')).toBeNull()
    })
  })

  it('shows monitor age and drift count when report ready with anomalies', async () => {
    const generatedAt = new Date().toISOString()
    mockGetMonitorReport.mockResolvedValue({
      enabled: true,
      ready: true,
      report: {
        ...emptyMonitorBinding().report,
        generatedAt,
        anomalies: [{ id: 'a1', kind: 'stale' }, { id: 'a2', kind: 'orphan' }],
      },
    })
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => {
      expect(screen.getByText(/monitor:.*ago/)).toBeDefined()
      expect(screen.getByText(/drift=2/)).toBeDefined()
    })
  })

  it('shows triage agent running badge', () => {
    mockAgentList.push({ id: 'a1', name: 'triage:task-1', taskId: 'task-1', state: 'running', costUsd: 0 })
    render(Orchestrator, { props: {} })
    expect(screen.getByText('1 running')).toBeDefined()
  })

  it('shows eval agent running badge', () => {
    mockAgentList.push({ id: 'a1', name: 'eval:task-1', taskId: 'task-1', state: 'running', costUsd: 0 })
    render(Orchestrator, { props: {} })
    expect(screen.getByText('1 running')).toBeDefined()
  })

  it('renders triage agent cards', () => {
    mockAgentList.push(
      { id: 'a1', name: 'triage:task-1', taskId: 'task-1', state: 'running', costUsd: 0.0001 },
      { id: 'a2', name: 'triage:task-2', taskId: 'task-2', state: 'stopped', costUsd: 0 },
    )
    render(Orchestrator, { props: {} })
    expect(screen.getByText('task-1')).toBeDefined()
    expect(screen.getByText('task-2')).toBeDefined()
    expect(screen.getByText('running')).toBeDefined()
    expect(screen.getByText('stopped')).toBeDefined()
  })

  it('renders eval agent cards', () => {
    mockAgentList.push(
      { id: 'a1', name: 'eval:task-1', taskId: 'task-1', state: 'running', costUsd: 0.05 },
    )
    render(Orchestrator, { props: {} })
    expect(screen.getByText('task-1')).toBeDefined()
    expect(screen.getByText('$0.0500')).toBeDefined()
  })

  it('only shows triage cost when above zero', () => {
    mockAgentList.push(
      { id: 'a1', name: 'triage:task-1', taskId: 'task-1', state: 'stopped', costUsd: 0 },
    )
    render(Orchestrator, { props: {} })
    expect(screen.queryByText('$0.0000')).toBeNull()
  })

  it('updates monitor binding when MonitorReport event fires', async () => {
    render(Orchestrator, { props: {} })
    await vi.waitFor(() => eventHandlers['monitor:report'])
    eventHandlers['monitor:report']({
      generatedAt: new Date().toISOString(),
      counts: emptyMonitorBinding().report.counts,
      anomalies: [{ id: 'a1', kind: 'stale' }],
      remediated: [],
      dispatched: [],
      issuesOpened: 0,
      issuesUpdated: 0,
    })
    await vi.waitFor(() => {
      expect(screen.getByText(/drift=1/)).toBeDefined()
    })
  })
})
