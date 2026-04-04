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

  it('renders all status columns', () => {
    mockByStatus.mockReturnValue([])
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Todo')).toBeDefined()
    expect(screen.getByText('Planning')).toBeDefined()
    expect(screen.getByText('In Progress')).toBeDefined()
    expect(screen.getByText('In Review')).toBeDefined()
    expect(screen.getByText('Human Required')).toBeDefined()
    expect(screen.getByText('Done')).toBeDefined()
  })

  it('calls byStatus for each column status', () => {
    mockByStatus.mockReturnValue([])
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(mockByStatus).not.toHaveBeenCalledWith('new')
    expect(mockByStatus).toHaveBeenCalledWith('todo')
    expect(mockByStatus).toHaveBeenCalledWith('planning')
    expect(mockByStatus).toHaveBeenCalledWith('plan-review') // merged into Planning column
    expect(mockByStatus).toHaveBeenCalledWith('in-progress')
    expect(mockByStatus).toHaveBeenCalledWith('in-review')
    expect(mockByStatus).toHaveBeenCalledWith('human-required')
    expect(mockByStatus).toHaveBeenCalledWith('done')
  })

  it('renders task cards in columns', () => {
    const tasks = [mockTask('t-1', 'First Task'), mockTask('t-2', 'Second Task')]
    mockByStatus.mockImplementation((status: string) =>
      status === 'plan-review' ? [] : tasks,
    )
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getAllByText('First Task')).toHaveLength(6)
  })
})
