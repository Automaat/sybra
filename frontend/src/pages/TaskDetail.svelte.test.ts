import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockGet = vi.fn()
const mockUpdate = vi.fn()
const mockRemove = vi.fn()
const mockStart = vi.fn()
const mockStop = vi.fn()
const mockByTask = vi.fn()
const mockUpdateAgent = vi.fn()
const mockEventsOn = vi.fn(() => vi.fn())

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

vi.mock('../../wailsjs/runtime/runtime.js', () => ({
  EventsOn: (...args: unknown[]) => mockEventsOn(...args),
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
    mockStart.mockReset()
    mockStop.mockReset()
    mockByTask.mockReturnValue(null)
    mockUpdateAgent.mockReset()
    mockEventsOn.mockReturnValue(vi.fn())
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
})
