import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    loading: false,
    error: '',
    list: [],
    tasks: new Map(),
    create: vi.fn(),
    update: vi.fn(),
  },
}))

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    list: [],
  },
}))

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: vi.fn(),
  },
}))

vi.mock('../components/TaskTimeline.svelte', () => ({ default: () => {} }))
vi.mock('../components/StatusPicker.svelte', () => ({ default: () => {} }))
vi.mock('../components/PriorityPicker.svelte', () => ({ default: () => {} }))
vi.mock('../components/AssignProjectDialog.svelte', () => ({ default: () => {} }))
vi.mock('../components/shell/MobileSheet.svelte', () => ({ default: () => {} }))

const { taskStore } = await import('../stores/tasks.svelte.js')
const { projectStore } = await import('../stores/projects.svelte.js')
const { notificationStore } = await import('../stores/notifications.svelte.js')
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
    Object.assign(taskStore, { loading: false, error: '', list: [], tasks: new Map(), create: vi.fn(), update: vi.fn() })
    Object.assign(projectStore, { list: [] })
    vi.mocked(notificationStore.pushLocal).mockClear()
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

  describe('submitInlineAdd error handling', () => {
    it('shows notification when task creation fails', async () => {
      vi.mocked(taskStore.create).mockRejectedValueOnce(new Error('Network error'))
      render(TaskList, { props: { onselect: vi.fn() } })

      const addBtn = screen.getAllByTitle('Add task (C)')[0]
      await fireEvent.click(addBtn)

      const input = screen.getByPlaceholderText('Task title')
      await fireEvent.input(input, { target: { value: 'New task' } })
      await fireEvent.keyDown(input, { key: 'Enter' })

      await new Promise((r) => setTimeout(r, 0))

      expect(notificationStore.pushLocal).toHaveBeenCalledWith('error', 'Create failed', 'Error: Network error')
    })
  })

  describe('view mode buttons', () => {
    it('renders List, Board, and Timeline buttons', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('List')).toBeDefined()
      expect(screen.getByText('Board')).toBeDefined()
      expect(screen.getByText('Timeline')).toBeDefined()
    })

    it('renders Show done checkbox', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByLabelText('Show done')).toBeDefined()
    })

    it('renders Logbook link', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Logbook →')).toBeDefined()
    })
  })

  describe('task selection', () => {
    it('calls onselect when task card is clicked in board view', async () => {
      const onselect = vi.fn()
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Selectable Task', 'todo')],
      })
      render(TaskList, { props: { onselect } })
      const taskCard = screen.getByText('Selectable Task')
      await fireEvent.click(taskCard)
      expect(onselect).toHaveBeenCalledWith('t-1')
    })
  })

  describe('show done tasks in list view', () => {
    it('hides done tasks in list view by default', async () => {
      Object.assign(taskStore, {
        list: [
          mockTask('t-done', 'Completed Task', 'done'),
          mockTask('t-todo', 'Active Task', 'todo'),
        ],
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      // Switch to list view
      await fireEvent.click(screen.getByText('List'))
      expect(screen.getByText('Active Task')).toBeDefined()
      expect(screen.queryByText('Completed Task')).toBeNull()
    })

    it('shows done tasks in list view when Show done checked', async () => {
      Object.assign(taskStore, {
        list: [
          mockTask('t-done', 'Completed Task', 'done'),
          mockTask('t-todo', 'Active Task', 'todo'),
        ],
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      // Switch to list view
      await fireEvent.click(screen.getByText('List'))
      const checkbox = screen.getByLabelText('Show done')
      await fireEvent.click(checkbox)
      expect(screen.getByText('Completed Task')).toBeDefined()
      expect(screen.getByText('Active Task')).toBeDefined()
    })
  })

  describe('move task error handling', () => {
    it('shows notification when task move fails', async () => {
      vi.mocked(taskStore.update).mockRejectedValueOnce(new Error('Server error'))
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Task to move', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Task to move', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn() } })

      const col = document.querySelector('[data-col-status="in-progress"]')
      if (col) {
        await fireEvent.drop(col, {
          dataTransfer: { getData: () => 't-1' },
        })
        await new Promise((r) => setTimeout(r, 0))
        expect(notificationStore.pushLocal).toHaveBeenCalledWith('error', 'Move failed', expect.any(String))
      }
    })
  })

  describe('project filter', () => {
    it('shows project filter when projects exist', () => {
      Object.assign(projectStore, {
        list: [{ id: 'p1', owner: 'myorg', repo: 'myrepo' }],
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('All projects')).toBeDefined()
    })

    it('does not show project filter when no projects', () => {
      Object.assign(projectStore, { list: [] })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText('All projects')).toBeNull()
    })
  })
})
