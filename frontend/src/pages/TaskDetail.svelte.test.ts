import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockGet = vi.fn()
const mockUpdate = vi.fn()
const mockRemove = vi.fn()
const mockStart = vi.fn()
const mockStop = vi.fn()
const mockByTask = vi.fn()
const mockUpdateAgent = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())
const mockPushLocal = vi.fn()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    get: (...args: unknown[]) => mockGet(...args),
    update: (...args: unknown[]) => mockUpdate(...args),
    remove: (...args: unknown[]) => mockRemove(...args),
  },
}))

vi.mock('../stores/agents.svelte.js', () => ({
  agentStore: {
    start: (...args: unknown[]) => mockStart(...args),
    stop: (...args: unknown[]) => mockStop(...args),
    byTask: (...args: unknown[]) => mockByTask(...args),
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
    mockGet.mockReset()
    mockUpdate.mockReset()
    mockRemove.mockReset()
    mockStart.mockReset()
    mockStop.mockReset()
    mockByTask.mockReturnValue(null)
    mockUpdateAgent.mockReset()
    mockEventsOn.mockReturnValue(vi.fn())
    mockPushLocal.mockReset()
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
      expect(screen.getByText('Delete')).toBeDefined()
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
      expect(screen.getByText('Delete')).toBeDefined()
    })
    screen.getByText('Delete').click()
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
      const copyBtn = screen.getByText('Copy ID')
      await fireEvent.click(copyBtn)
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
      const copyBtn = screen.getByText('Copy ID')
      await fireEvent.click(copyBtn)
      await vi.waitFor(() => {
        expect(mockPushLocal).toHaveBeenCalledWith('error', 'Copy failed', expect.any(String))
      })
    })
  })
})
