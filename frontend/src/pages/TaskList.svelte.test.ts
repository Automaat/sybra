import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockByStatus = vi.fn()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    loading: false,
    error: '',
    byStatus: (...args: unknown[]) => mockByStatus(...args),
    list: [],
  },
}))

vi.mock('@skeletonlabs/skeleton-svelte', () => {
  return {
    SegmentedControl: Object.assign(() => {}, {
      Control: () => {},
      Indicator: () => {},
      Item: Object.assign(() => {}, {
        ItemText: () => {},
        ItemHiddenInput: () => {},
      }),
    }),
  }
})

const { taskStore } = await import('../stores/tasks.svelte.js')
const TaskList = (await import('./TaskList.svelte')).default

const mockTask = (id: string, title: string) => ({
  id,
  title,
  status: 'todo',
  agentMode: 'headless',
  allowedTools: [],
  tags: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  body: '',
})

describe('TaskList', () => {
  beforeEach(() => {
    Object.assign(taskStore, { loading: false, error: '' })
    mockByStatus.mockReturnValue([])
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('shows loading message when taskStore.loading is true', () => {
    Object.assign(taskStore, { loading: true })
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Loading tasks...')).toBeDefined()
  })

  it('shows error message when taskStore.error is set', () => {
    Object.assign(taskStore, { error: 'Failed to load' })
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Failed to load')).toBeDefined()
  })

  it('shows empty state when no tasks', () => {
    mockByStatus.mockReturnValue([])
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('No tasks')).toBeDefined()
    expect(screen.getByText('Create a task to get started')).toBeDefined()
  })

  it('renders task cards when tasks exist', () => {
    const tasks = [mockTask('t-1', 'First Task'), mockTask('t-2', 'Second Task')]
    mockByStatus.mockReturnValue(tasks)
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('First Task')).toBeDefined()
    expect(screen.getByText('Second Task')).toBeDefined()
  })

  it('calls byStatus with default "all" filter', () => {
    mockByStatus.mockReturnValue([])
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(mockByStatus).toHaveBeenCalledWith('all')
  })
})
