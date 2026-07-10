import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockUpdate = vi.fn()
const mockRemove = vi.fn()
const mockStartReview = vi.fn()
const mockStartFixReview = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    update: (...args: unknown[]) => mockUpdate(...args),
    remove: (...args: unknown[]) => mockRemove(...args),
  },
}))

vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: {
    list: [],
  },
}))

vi.mock('../../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: vi.fn(),
  },
}))

vi.mock('$lib/api', () => ({
  StartReview: (...args: unknown[]) => mockStartReview(...args),
  StartFixReview: (...args: unknown[]) => mockStartFixReview(...args),
}))

const TaskHeaderBar = (await import('./TaskHeaderBar.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'Test Task',
  status: 'todo',
  taskType: 'normal',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  projectId: 'foo/bar',
  prNumber: 0,
  reviewed: false,
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('TaskHeaderBar', () => {
  beforeEach(() => {
    mockUpdate.mockReset()
    mockRemove.mockReset()
    mockStartReview.mockReset()
    mockStartFixReview.mockReset()
    mockUpdate.mockResolvedValue(baseTask)
    mockRemove.mockResolvedValue(undefined)
    vi.spyOn(window, 'confirm').mockReturnValue(true)
  })
  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('renders title', () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    expect(screen.getByText('Test Task')).toBeDefined()
  })

  it('opens title editor on click', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    await fireEvent.click(screen.getByText('Test Task'))
    expect(screen.getByDisplayValue('Test Task')).toBeDefined()
  })

  it('saves title via Enter', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    await fireEvent.click(screen.getByText('Test Task'))
    const input = screen.getByDisplayValue('Test Task') as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Renamed' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('t1', { title: 'Renamed' })
    })
  })

  it('updates status via select onchange', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    const select = screen.getByTestId('task-status-select') as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'in-progress' } })
    expect(mockUpdate).toHaveBeenCalledWith('t1', { status: 'in-progress' })
  })

  it('updates task type via the select in the overflow menu', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    // Task type is demoted out of the action cluster into the overflow menu.
    await fireEvent.click(screen.getByLabelText('More actions'))
    const select = screen.getByTestId('task-type-select') as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'debug' } })
    expect(mockUpdate).toHaveBeenCalledWith('t1', { task_type: 'debug' })
  })

  it('shows Fix Review button on in-review status with prNumber', () => {
    render(TaskHeaderBar, {
      props: {
        task: { ...baseTask, status: 'in-review', prNumber: 42 } as never,
        ondelete: vi.fn(),
      },
    })
    expect(screen.getByText('Fix Review Comments')).toBeDefined()
  })

  it('shows Run Review on review-tagged task with prNumber', () => {
    render(TaskHeaderBar, {
      props: {
        task: { ...baseTask, tags: ['review'], prNumber: 42 } as never,
        ondelete: vi.fn(),
      },
    })
    expect(screen.getByText('Run Review')).toBeDefined()
  })

  it('calls remove and ondelete on Delete from the overflow menu', async () => {
    const ondelete = vi.fn()
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete } })
    await fireEvent.click(screen.getByLabelText('More actions'))
    await fireEvent.click(screen.getByText('Delete task'))
    await waitFor(() => {
      expect(window.confirm).toHaveBeenCalledWith('Delete task "Test Task"? This cannot be undone.')
      expect(mockRemove).toHaveBeenCalledWith('t1')
      expect(ondelete).toHaveBeenCalled()
    })
  })

  it('does not delete when confirmation is cancelled', async () => {
    vi.mocked(window.confirm).mockReturnValue(false)
    const ondelete = vi.fn()
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete } })
    await fireEvent.click(screen.getByLabelText('More actions'))
    await fireEvent.click(screen.getByText('Delete task'))
    expect(mockRemove).not.toHaveBeenCalled()
    expect(ondelete).not.toHaveBeenCalled()
  })

  it('responds to task-detail:edit-title window event', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    window.dispatchEvent(new CustomEvent('task-detail:edit-title'))
    await waitFor(() => {
      expect(screen.getByDisplayValue('Test Task')).toBeDefined()
    })
  })

  it('shows reviewed badge when task.reviewed is true', () => {
    render(TaskHeaderBar, {
      props: {
        task: { ...baseTask, reviewed: true } as never,
        ondelete: vi.fn(),
      },
    })
    expect(screen.getByText(/Reviewed/)).toBeDefined()
  })

  it('shows Re-run Review when review-tagged + reviewed', () => {
    render(TaskHeaderBar, {
      props: {
        task: { ...baseTask, tags: ['review'], prNumber: 42, reviewed: true } as never,
        ondelete: vi.fn(),
      },
    })
    expect(screen.getByText('Re-run Review')).toBeDefined()
  })

  it('calls StartFixReview when Fix Review Comments clicked', async () => {
    mockStartFixReview.mockResolvedValue(undefined)
    render(TaskHeaderBar, {
      props: {
        task: { ...baseTask, status: 'in-review', prNumber: 42 } as never,
        ondelete: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByText('Fix Review Comments'))
    await waitFor(() => {
      expect(mockStartFixReview).toHaveBeenCalledWith('t1')
    })
  })

  it('calls StartReview when Run Review clicked', async () => {
    mockStartReview.mockResolvedValue(undefined)
    render(TaskHeaderBar, {
      props: {
        task: { ...baseTask, tags: ['review'], prNumber: 42 } as never,
        ondelete: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByText('Run Review'))
    await waitFor(() => {
      expect(mockStartReview).toHaveBeenCalledWith('t1')
    })
  })

  it('Copy ID / Copy branch live in the overflow menu', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    // Utility actions are not in the always-visible action row.
    expect(screen.queryByText('Copy ID')).toBeNull()
    await fireEvent.click(screen.getByLabelText('More actions'))
    expect(screen.getByText('Copy ID')).toBeDefined()
    expect(screen.getByText('Copy branch')).toBeDefined()
  })

  it('closes the overflow menu on Escape', async () => {
    render(TaskHeaderBar, { props: { task: baseTask as never, ondelete: vi.fn() } })
    await fireEvent.click(screen.getByLabelText('More actions'))
    expect(screen.getByText('Copy ID')).toBeDefined()
    await fireEvent.keyDown(window, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByText('Copy ID')).toBeNull())
  })

  it('hides Copy branch in the menu when the task has no project', async () => {
    render(TaskHeaderBar, { props: { task: { ...baseTask, projectId: '' } as never, ondelete: vi.fn() } })
    await fireEvent.click(screen.getByLabelText('More actions'))
    expect(screen.getByText('Copy ID')).toBeDefined()
    expect(screen.queryByText('Copy branch')).toBeNull()
  })
})
