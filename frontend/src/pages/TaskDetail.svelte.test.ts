import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { SvelteMap } from 'svelte/reactivity'

const mockTasksMap = new SvelteMap<string, any>()
const mockGet = vi.fn()
const mockUpdate = vi.fn()
const mockRemove = vi.fn()
const mockDispatchFromHumanRequired = vi.fn()
const mockStart = vi.fn()
const mockStop = vi.fn()
const mockByTask = vi.fn()
const mockByState = vi.fn()
const mockUpdateAgent = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())
const mockPushLocal = vi.fn()
const mockListTaskProgress = vi.fn()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    tasks: mockTasksMap,
    get list() {
      return [...mockTasksMap.values()]
    },
    get: async (id: string) => {
      const result = await mockGet(id)
      mockTasksMap.set(id, result)
      return result
    },
    update: (...args: unknown[]) => mockUpdate(...args),
    remove: (...args: unknown[]) => mockRemove(...args),
    dispatchFromHumanRequired: async (...args: [string, string, string]) => {
      const result = await mockDispatchFromHumanRequired(...args)
      mockTasksMap.set(result.id, result)
      return result
    },
  },
}))

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    start: (...args: unknown[]) => mockStart(...args),
    stop: (...args: unknown[]) => mockStop(...args),
    byTask: (...args: unknown[]) => mockByTask(...args),
    byState: (...args: unknown[]) => mockByState(...args),
    updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
  },
}))

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: (...args: unknown[]) => mockPushLocal(...args),
  },
}))

vi.mock('../stores/reviews.svelte.js', () => ({
  reviewStore: {
    createdByMe: [],
    reviewRequested: [],
    loading: false,
    error: '',
    totalCount: 0,
    allPRs: [],
    byTask: vi.fn(() => []),
    byRepo: vi.fn(() => []),
    load: vi.fn(),
  },
}))

vi.mock('../stores/connection.svelte.js', () => ({
  connectionStore: {
    online: true,
    backendOnline: true,
    networkOnline: true,
    start: vi.fn(() => () => {}),
  },
}))

vi.mock('$lib/api', () => ({
  EventsOn: (...args: any[]) => mockEventsOn(...args),
  BrowserOpenURL: vi.fn(),
  StartFixReview: vi.fn(),
  StartReview: vi.fn(),
  GetAgentRunLog: vi.fn(),
  GetAgentRunConvoLog: vi.fn(),
  ListTaskArtifacts: vi.fn(async () => []),
  GetTaskSetupLog: vi.fn(async () => ({ taskId: 'task-1', exists: false })),
  ListTaskAuditEvents: vi.fn(async () => []),
  ListTaskProgress: (...args: unknown[]) => mockListTaskProgress(...args),
}))

vi.mock('@skeletonlabs/skeleton-svelte', () => ({
  SegmentedControl: Object.assign(() => {}, {
    Control: () => {},
    Indicator: () => {},
    Item: Object.assign(() => {}, {
      ItemText: () => {},
      ItemHiddenInput: () => {},
    }),
  }),
}))

vi.mock('../components/StreamOutput.svelte', () => ({ default: () => {} }))
vi.mock('../components/StatusBadge.svelte', () => ({ default: () => {} }))
vi.mock('../components/ChatView.svelte', () => ({ default: () => {} }))
vi.mock('../components/MessageBubble.svelte', () => ({ default: () => {} }))
vi.mock('../components/ProviderLogo.svelte', () => ({ default: () => {} }))

const TaskDetail = (await import('./TaskDetail.svelte')).default

const mockTask = {
  id: 'task-1',
  title: 'Test Task',
  status: 'todo',
  agentMode: 'headless',
  allowedTools: [],
  tags: ['backend'],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  body: 'Task body',
}

describe('TaskDetail', () => {
  beforeEach(() => {
    mockTasksMap.clear()
    mockGet.mockReset()
    mockUpdate.mockReset()
    mockRemove.mockReset()
    mockDispatchFromHumanRequired.mockReset()
    mockStart.mockReset()
    mockStop.mockReset()
    mockByTask.mockReturnValue(null)
    mockByState.mockReturnValue([])
    mockUpdateAgent.mockReset()
    mockEventsOn.mockReturnValue(vi.fn())
    mockPushLocal.mockReset()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockListTaskProgress.mockReset()
    mockListTaskProgress.mockResolvedValue([])
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('shows loading initially before loadTask resolves', () => {
    mockGet.mockReturnValue(new Promise(() => {}))
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    expect(screen.getByText('Loading...')).toBeDefined()
  })

  it('shows task title after loading', async () => {
    mockGet.mockResolvedValue(mockTask)
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Test Task')).toBeDefined()
    })
  })

  it('shows error when loadTask fails', async () => {
    mockGet.mockRejectedValue(new Error('not found'))
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Error: not found')).toBeDefined()
    })
  })

  it('shows back to tasks button', () => {
    mockGet.mockReturnValue(new Promise(() => {}))
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    expect(screen.getByText('Back to tasks')).toBeDefined()
  })

  it('shows delete button after loading', async () => {
    mockGet.mockResolvedValue(mockTask)
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByLabelText('More actions')).toBeDefined()
    })
  })

  it('calls remove and ondelete when delete clicked', async () => {
    mockGet.mockResolvedValue(mockTask)
    mockRemove.mockResolvedValue(undefined)
    const ondelete = vi.fn()
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete },
    })
    await vi.waitFor(() => {
      expect(screen.getByLabelText('More actions')).toBeDefined()
    })
    screen.getByLabelText('More actions').click()
    await vi.waitFor(() => {
      expect(screen.getByText('Delete task')).toBeDefined()
    })
    screen.getByText('Delete task').click()
    await vi.waitFor(() => {
      expect(mockRemove).toHaveBeenCalledWith('task-1')
      expect(ondelete).toHaveBeenCalled()
    })
  })

  it('shows start agent button with mode', async () => {
    mockGet.mockResolvedValue(mockTask)
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Start agent')).toBeDefined()
    })
  })

  it('calls onback when back button is clicked', async () => {
    mockGet.mockResolvedValue(mockTask)
    const onback = vi.fn()
    render(TaskDetail, {
      props: { taskId: 'task-1', onback, onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await fireEvent.click(screen.getByText('Back to tasks'))
    expect(onback).toHaveBeenCalled()
  })

  it('renders task tags after loading', async () => {
    mockGet.mockResolvedValue({ ...mockTask, tags: ['backend', 'auth'] })
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('backend')).toBeDefined()
      expect(screen.getByText('auth')).toBeDefined()
    })
  })

  it('renders task with no tags without crashing', async () => {
    mockGet.mockResolvedValue({ ...mockTask, tags: [] })
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Test Task')).toBeDefined()
    })
  })

  it('shows status select with current task status', async () => {
    mockGet.mockResolvedValue({ ...mockTask, status: 'in-progress' })
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      const select = document.querySelector('[data-testid="task-status-select"]') as HTMLSelectElement
      expect(select).toBeDefined()
      expect(select?.value).toBe('in-progress')
    })
  })

  it('calls taskStore.update when status select changes', async () => {
    mockGet.mockResolvedValue({ ...mockTask, status: 'todo' })
    mockUpdate.mockResolvedValue({ ...mockTask, status: 'in-progress' })
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Test Task')).toBeDefined()
    })
    const select = document.querySelector('[data-testid="task-status-select"]') as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'in-progress' } })
    await vi.waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('task-1', { status: 'in-progress' })
    })
  })

  it('does not call taskStore.update when status unchanged', async () => {
    mockGet.mockResolvedValue({ ...mockTask, status: 'todo' })
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Test Task')).toBeDefined()
    })
    const select = document.querySelector('[data-testid="task-status-select"]') as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'todo' } })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('shows interactive agent mode in task with interactive mode', async () => {
    mockGet.mockResolvedValue({ ...mockTask, agentMode: 'interactive' })
    render(TaskDetail, {
      props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
    })
    await vi.waitFor(() => {
      expect(screen.getByText('Start agent')).toBeDefined()
    })
  })

  describe('tabs', () => {
    it('defaults to the Overview tab (metadata visible)', async () => {
      mockGet.mockResolvedValue(mockTask)
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Mode')).toBeDefined()
      })
    })

    it('shows the read-only Plan panel when the task has a plan', async () => {
      mockGet.mockResolvedValue({ ...mockTask, status: 'done', plan: '# Plan\n\ndo it' })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        // TaskPlanPanel renders a static "read-only" marker beside the plan.
        expect(screen.getByText('read-only')).toBeDefined()
      })
    })

    it('shows the Review panel when the task has a code review', async () => {
      mockGet.mockResolvedValue({ ...mockTask, status: 'done', codeReview: '# Code Review\n\nlgtm' })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('auto-generated')).toBeDefined()
      })
    })

    it('pins a live running agent above the tabs', async () => {
      mockByState.mockReturnValue([{ id: 'a1', state: 'running', mode: 'headless', taskId: 'task-1' }])
      mockGet.mockResolvedValue({ ...mockTask, status: 'in-progress' })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Stop')).toBeDefined()
      })
    })

    it('offers a Plan tab with approve/reject for plan-review, even with no plan sidecar', async () => {
      // A plan-review task keeps its decision even when the plan is in the body.
      mockGet.mockResolvedValue({ ...mockTask, status: 'plan-review' })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Approve Plan')).toBeDefined()
        expect(screen.getByText('Reject Plan')).toBeDefined()
      })
    })

    it('shows no Children panel for a task with no umbrella/dependsOn relationships', async () => {
      mockGet.mockResolvedValue(mockTask)
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Test Task')).toBeDefined()
      })
      expect(screen.queryByText('No child tasks linked yet.')).toBeNull()
    })

    it('does not show Children for a normal task that has prerequisites', async () => {
      mockGet.mockResolvedValue({
        ...mockTask,
        taskType: 'normal',
        dependsOn: ['https://github.com/Automaat/sybra/issues/10'],
      })
      mockTasksMap.set('prereq-1', {
        id: 'prereq-1',
        title: 'Prerequisite task',
        status: 'todo',
        agentMode: 'headless',
        tags: [],
        issue: 'https://github.com/Automaat/sybra/issues/10',
        createdAt: '2026-04-01T00:00:00Z',
        updatedAt: '2026-04-01T00:00:00Z',
      })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Test Task')).toBeDefined()
      })
      expect(screen.getByTestId('task-detail-tabs').getAttribute('data-tab-labels')).not.toContain('Children')
      expect(screen.queryByText('Prerequisite task')).toBeNull()
      expect(screen.queryByText('No child tasks linked yet.')).toBeNull()
    })

    it('renders the Children panel with materialized children plus unresolved refs for an umbrella', async () => {
      mockGet.mockResolvedValue({
        ...mockTask,
        taskType: 'umbrella',
        issue: 'https://github.com/Automaat/sybra/issues/1213',
        dependsOn: [
          'https://github.com/Automaat/sybra/issues/10',
          'https://github.com/Automaat/sybra/issues/99',
        ],
      })
      mockTasksMap.set('child-1', {
        id: 'child-1',
        title: 'Child task',
        status: 'todo',
        agentMode: 'headless',
        tags: [],
        issue: 'https://github.com/Automaat/sybra/issues/10',
        umbrellaIssue: 'https://github.com/Automaat/sybra/issues/1213',
        createdAt: '2026-04-01T00:00:00Z',
        updatedAt: '2026-04-01T00:00:00Z',
      })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByTestId('task-detail-tabs').getAttribute('data-tab-labels')).toContain('Children · 2')
        expect(screen.getByText('Child task')).toBeDefined()
        expect(screen.getByText('automaat/sybra#99')).toBeDefined()
      })
    })

  })

  describe('human-required dispatch', () => {
    it('reflects the new status after a successful dispatch', async () => {
      mockGet.mockResolvedValue({ ...mockTask, status: 'human-required', statusReason: 'needs a decision' })
      mockDispatchFromHumanRequired.mockResolvedValue({ ...mockTask, status: 'testing', statusReason: '' })
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByPlaceholderText('Decision reason (required)...')).toBeDefined()
      })
      const textarea = screen.getByPlaceholderText('Decision reason (required)...')
      await fireEvent.input(textarea, { target: { value: 'looks fine, proceed' } })
      await fireEvent.click(screen.getByText('Send to testing'))
      await vi.waitFor(() => {
        expect(mockDispatchFromHumanRequired).toHaveBeenCalledWith('task-1', 'testing', 'looks fine, proceed')
      })
      await vi.waitFor(() => {
        expect(screen.queryByPlaceholderText('Decision reason (required)...')).toBeNull()
      })
    })
  })

  describe('copyId', () => {
    beforeEach(() => {
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: vi.fn() },
        configurable: true,
        writable: true,
      })
    })

    it('copies task id to clipboard', async () => {
      vi.mocked(navigator.clipboard.writeText).mockResolvedValueOnce(undefined)
      mockGet.mockResolvedValue(mockTask)
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Test Task')).toBeDefined()
      })
      await fireEvent.click(screen.getByLabelText('More actions'))
      await fireEvent.click(screen.getByText('Copy ID'))
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('task-1')
    })

    it('shows notification when clipboard write fails', async () => {
      vi.mocked(navigator.clipboard.writeText).mockRejectedValueOnce(new Error('denied'))
      mockGet.mockResolvedValue(mockTask)
      render(TaskDetail, {
        props: { taskId: 'task-1', onback: vi.fn(), onviewagent: vi.fn(), ondelete: vi.fn() },
      })
      await vi.waitFor(() => {
        expect(screen.getByText('Test Task')).toBeDefined()
      })
      await fireEvent.click(screen.getByLabelText('More actions'))
      await fireEvent.click(screen.getByText('Copy ID'))
      await vi.waitFor(() => {
        expect(mockPushLocal).toHaveBeenCalledWith('error', 'Copy failed', expect.any(String))
      })
    })
  })
})
