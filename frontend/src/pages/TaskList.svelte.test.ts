import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    loading: false,
    error: '',
    list: [],
  },
}))

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    list: [],
  },
}))

const { taskStore } = await import('../stores/tasks.svelte.js')
const TaskList = (await import('./TaskList.svelte')).default

const mockTask = (id: string, title: string, status = 'todo') => ({
  id,
  title,
  status,
  agentMode: 'headless',
  allowedTools: [],
  tags: [],
  projectId: '',
  issue: '',
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  body: '',
})

describe('TaskList', () => {
  beforeEach(() => {
    Object.assign(taskStore, { loading: false, error: '', list: [] })
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

  it('renders visible status columns (Done hidden by default)', () => {
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('Todo')).toBeDefined()
    expect(screen.getByText('Planning')).toBeDefined()
    expect(screen.getByText('In Progress')).toBeDefined()
    expect(screen.getByText('In Review')).toBeDefined()
    expect(screen.getByText('Human Required')).toBeDefined()
    expect(screen.queryByText(/^Done$/)).toBeNull()
  })

  it('renders task cards in correct columns', () => {
    Object.assign(taskStore, {
      list: [
        mockTask('t-1', 'First Task', 'todo'),
        mockTask('t-2', 'Second Task', 'in-progress'),
      ],
    })
    render(TaskList, { props: { onselect: vi.fn() } })
    expect(screen.getByText('First Task')).toBeDefined()
    expect(screen.getByText('Second Task')).toBeDefined()
  })
})
