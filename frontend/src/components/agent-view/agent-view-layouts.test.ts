import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { Agent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'

// jsdom doesn't implement Web Animations API used by svelte/transition
if (!Element.prototype.animate) {
  Element.prototype.animate = vi.fn(() => ({
    onfinish: null,
    cancel: vi.fn(),
    finish: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  })) as any
}

const mockOpenWorktree = vi.fn()
const mockSummarize = vi.fn()

vi.mock('$lib/api.js', () => ({
  OpenWorktree: (...args: unknown[]) => mockOpenWorktree(...args),
}))

vi.mock('$lib/agent-summary.js', () => ({
  summarizeAgent: (...args: unknown[]) => mockSummarize(...args),
}))

vi.mock('../ChatView.svelte', () => ({ default: () => {} }))
vi.mock('../StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../ActionTimeline.svelte', () => ({ default: () => {} }))
vi.mock('../ToolApproval.svelte', () => ({ default: () => {} }))
vi.mock('./SessionWorkspace.svelte', () => ({ default: () => {} }))
vi.mock('./AgentSidebarList.svelte', () => ({ default: () => {} }))
// ThreePanelLayout and ChatInput are left real (not mocked): RunningLayout's
// steer box renders through them, and the steer tests below exercise the
// actual DOM they produce (textarea, send button, error text).

const mockConvoStore = {
  pendingApprovals: new Map<string, any>(),
  respondApproval: vi.fn(),
  sendMessage: vi.fn(),
}

vi.mock('../../stores/convo.svelte.js', () => ({
  convoStore: mockConvoStore,
}))

const QueuedLayout = (await import('./QueuedLayout.svelte')).default
const DoneLayout = (await import('./DoneLayout.svelte')).default
const ReviewingLayout = (await import('./ReviewingLayout.svelte')).default
const RunningLayout = (await import('./RunningLayout.svelte')).default

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    taskId: 'task-1',
    mode: 'headless',
    state: 'done',
    sessionId: '',
    costUsd: 0,
    startedAt: '2026-01-01T10:00:00Z',
    lastEventAt: '2026-01-01T10:05:00Z',
    external: false,
    ...overrides,
  }
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return Task.createFrom({
    id: 'task-1',
    title: 'Test task',
    status: 'done',
    taskType: '',
    agentMode: 'headless',
    allowedTools: [],
    tags: [],
    projectId: '',
    branch: '',
    prNumber: 0,
    issue: '',
    statusReason: '',
    body: '',
    plan: '',
    planCritique: '',
    slug: '',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-15T00:00:00Z',
    dueDate: '',
    closedAt: '',
    ...overrides,
  })
}

describe('QueuedLayout', () => {
  afterEach(() => { cleanup() })

  it('shows "Agent starting…" badge', () => {
    render(QueuedLayout, { props: { linkedTask: null } })
    expect(screen.getByText('Agent starting…')).toBeDefined()
  })

  it('shows task title when linkedTask provided', () => {
    const t = makeTask({ title: 'My important task' })
    render(QueuedLayout, { props: { linkedTask: t } })
    expect(screen.getByText('My important task')).toBeDefined()
  })

  it('shows task body when provided', () => {
    const t = makeTask({ title: 'T', body: 'Task body text' })
    render(QueuedLayout, { props: { linkedTask: t } })
    expect(screen.getByText('Task body text')).toBeDefined()
  })

  it('shows no description placeholder when body empty', () => {
    const t = makeTask({ title: 'T', body: '' })
    render(QueuedLayout, { props: { linkedTask: t } })
    expect(screen.getByText('No description provided.')).toBeDefined()
  })

  it('shows waiting message when no linkedTask', () => {
    render(QueuedLayout, { props: { linkedTask: null } })
    expect(screen.getByText("Agent hasn't started producing output yet.")).toBeDefined()
  })
})

describe('DoneLayout', () => {
  beforeEach(() => {
    mockSummarize.mockReturnValue({
      finalMessage: '',
      filesEdited: [],
      commandsRun: 0,
    })
  })

  afterEach(() => { cleanup() })

  it('shows "Done" badge', () => {
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('Done')).toBeDefined()
  })

  it('shows final message from summary', () => {
    mockSummarize.mockReturnValue({ finalMessage: 'Task completed successfully', filesEdited: [], commandsRun: 0 })
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('Task completed successfully')).toBeDefined()
  })

  it('shows cost when costUsd > 0', () => {
    render(DoneLayout, {
      props: { a: makeAgent({ costUsd: 0.042 }), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('$0.042')).toBeDefined()
  })

  it('shows duration', () => {
    render(DoneLayout, {
      props: {
        a: makeAgent({ startedAt: '2026-01-01T10:00:00Z', lastEventAt: '2026-01-01T10:02:30Z' }),
        linkedTask: null,
        streamOutputs: [],
        convoEvents: [],
      },
    })
    expect(screen.getByText('2m 30s')).toBeDefined()
  })

  it('shows branch when linkedTask has branch', () => {
    const t = makeTask({ branch: 'feat/my-feature' })
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('feat/my-feature')).toBeDefined()
  })

  it('shows PR number when linkedTask has prNumber', () => {
    const t = makeTask({ prNumber: 42 })
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('#42')).toBeDefined()
  })

  it('shows files changed when filesEdited non-empty', () => {
    mockSummarize.mockReturnValue({ finalMessage: '', filesEdited: ['src/foo.ts', 'src/bar.ts'], commandsRun: 0 })
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('src/foo.ts')).toBeDefined()
    expect(screen.getByText('src/bar.ts')).toBeDefined()
  })

  it('shows no files message when filesEdited empty', () => {
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('Agent completed without modifying files.')).toBeDefined()
  })

  it('toggles activity section on button click', async () => {
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('Show full activity')).toBeDefined()
    await fireEvent.click(screen.getByText('Show full activity'))
    expect(screen.getByText('Hide activity')).toBeDefined()
  })

  it('shows commands run count when > 0', () => {
    mockSummarize.mockReturnValue({ finalMessage: '', filesEdited: [], commandsRun: 5 })
    render(DoneLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('5')).toBeDefined()
  })

  it('shows turn count when turnCount > 0', () => {
    render(DoneLayout, {
      props: { a: makeAgent({ turnCount: 12 }), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('12')).toBeDefined()
  })
})

describe('ReviewingLayout', () => {
  beforeEach(() => {
    mockSummarize.mockReturnValue({ finalMessage: '', filesEdited: [], commandsRun: 0 })
    mockOpenWorktree.mockResolvedValue(undefined)
  })

  afterEach(() => { cleanup() })

  it('shows "In Review" badge', () => {
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('In Review')).toBeDefined()
  })

  it('shows final message when summary has one', () => {
    mockSummarize.mockReturnValue({ finalMessage: 'PR ready for review', filesEdited: [], commandsRun: 0 })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('PR ready for review')).toBeDefined()
  })

  it('shows branch when linkedTask has branch', () => {
    const t = makeTask({ branch: 'fix/auth-bug' })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('fix/auth-bug')).toBeDefined()
  })

  it('shows PR number when linkedTask has prNumber', () => {
    const t = makeTask({ prNumber: 99 })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('PR #99')).toBeDefined()
  })

  it('shows Open worktree button when task has id', () => {
    const t = makeTask({ id: 'task-abc' })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('Open worktree')).toBeDefined()
  })

  it('calls OpenWorktree when button clicked', async () => {
    const t = makeTask({ id: 'task-abc' })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    await fireEvent.click(screen.getByText('Open worktree'))
    expect(mockOpenWorktree).toHaveBeenCalledWith('task-abc')
  })

  it('shows no branch/PR message when both missing', () => {
    const t = makeTask({ branch: '', prNumber: 0 })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('No branch or PR recorded — open the linked task for context.')).toBeDefined()
  })

  it('shows error when OpenWorktree fails', async () => {
    mockOpenWorktree.mockRejectedValue(new Error('not found'))
    const t = makeTask({ id: 'task-xyz' })
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: t, streamOutputs: [], convoEvents: [] },
    })
    await fireEvent.click(screen.getByText('Open worktree'))
    await vi.waitFor(() => {
      expect(screen.getByText('Error: not found')).toBeDefined()
    })
  })

  it('toggles activity on button click', async () => {
    render(ReviewingLayout, {
      props: { a: makeAgent(), linkedTask: null, streamOutputs: [], convoEvents: [] },
    })
    expect(screen.getByText('Show full activity')).toBeDefined()
    await fireEvent.click(screen.getByText('Show full activity'))
    expect(screen.getByText('Hide activity')).toBeDefined()
  })
})

describe('RunningLayout steer box', () => {
  beforeEach(() => {
    mockConvoStore.sendMessage.mockReset()
  })

  afterEach(() => { cleanup() })

  function runningLayoutProps(overrides: Partial<Agent> = {}) {
    return {
      a: makeAgent({ mode: 'headless', provider: 'claude', state: 'running', canSteer: true, ...overrides }),
      planSteps: [],
      timelineEntries: [],
      selectedIndex: null,
      onselect: vi.fn(),
      streamOutputs: [],
      convoEvents: [],
      allAgents: [],
      latestToolUse: undefined,
      onnavigate: vi.fn(),
    }
  }

  it('shows the steer box for a running Claude headless agent', () => {
    render(RunningLayout, { props: runningLayoutProps() })
    expect(screen.getByText('Steer agent')).toBeDefined()
    expect(screen.getByPlaceholderText('Send guidance to the running agent...')).toBeDefined()
    expect(screen.getByText('Pause')).toBeDefined()
  })

  it('hides the steer box for a non-Claude headless agent', () => {
    render(RunningLayout, { props: runningLayoutProps({ provider: 'codex', canSteer: false }) })
    expect(screen.queryByText('Steer agent')).toBeNull()
  })

  it('hides the steer box once the agent is no longer running', () => {
    render(RunningLayout, { props: runningLayoutProps({ state: 'paused', canSteer: false }) })
    expect(screen.queryByText('Steer agent')).toBeNull()
  })

  it('sends guidance through convoStore.sendMessage', async () => {
    mockConvoStore.sendMessage.mockResolvedValue(undefined)
    const props = runningLayoutProps()
    render(RunningLayout, { props })

    const textarea = screen.getByPlaceholderText('Send guidance to the running agent...')
    await fireEvent.input(textarea, { target: { value: 'focus on the auth bug' } })
    await fireEvent.click(screen.getByTitle('Send message'))

    await vi.waitFor(() => {
      expect(mockConvoStore.sendMessage).toHaveBeenCalledWith(props.a.id, 'focus on the auth bug')
    })
  })

  it('surfaces a rejected send instead of silently clearing the text', async () => {
    mockConvoStore.sendMessage.mockRejectedValue(new Error('agent is finalizing'))
    const props = runningLayoutProps()
    render(RunningLayout, { props })

    const textarea = screen.getByPlaceholderText('Send guidance to the running agent...') as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'one more thing' } })
    await fireEvent.click(screen.getByTitle('Send message'))

    await vi.waitFor(() => {
      expect(screen.getByText('agent is finalizing')).toBeDefined()
    })
    expect(textarea.value).toBe('one more thing')
  })
})
