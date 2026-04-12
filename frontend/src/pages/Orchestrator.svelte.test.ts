import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockIsOrchestratorRunning = vi.fn().mockResolvedValue(false)
const mockStartOrchestrator = vi.fn().mockResolvedValue(undefined)
const mockStopOrchestrator = vi.fn().mockResolvedValue(undefined)
const mockGetOrchestratorAgentID = vi.fn().mockResolvedValue('')
const mockEventsOn = vi.fn().mockReturnValue(vi.fn())

const mockAgentList: any[] = []

vi.mock('$lib/api', () => ({
  IsOrchestratorRunning: (...args: unknown[]) => mockIsOrchestratorRunning(...args),
  StartOrchestrator: (...args: unknown[]) => mockStartOrchestrator(...args),
  StopOrchestrator: (...args: unknown[]) => mockStopOrchestrator(...args),
  GetOrchestratorAgentID: (...args: unknown[]) => mockGetOrchestratorAgentID(...args),
  EventsOn: (...args: any[]) => mockEventsOn(...args),
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
    conversations: new Map(),
    getOutput: vi.fn().mockResolvedValue([]),
    subscribe: vi.fn().mockReturnValue(() => {}),
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
    mockEventsOn.mockReturnValue(vi.fn())
    mockAgentList.length = 0
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

  it('shows triage agent running badge when triage agent is running', () => {
    mockAgentList.push({ id: 'a1', name: 'triage:task-1', taskId: 'task-1', state: 'running', costUsd: 0 })
    render(Orchestrator, { props: {} })
    expect(screen.getByText('1 running')).toBeDefined()
  })
})
