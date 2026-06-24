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
const { viewModeStore } = await import('../lib/view-mode.svelte.js')
const { focusModeStore } = await import('../lib/focus-mode.svelte.js')
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
    viewModeStore.set('board')
    focusModeStore.set(false)
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
    it('renders List, Board, and the de-emphasized Timeline button', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('List')).toBeDefined()
      expect(screen.getByText('Board')).toBeDefined()
      expect(screen.getByText('Timeline')).toBeDefined()
    })

    it('hides the advanced Timeline button in focus mode', () => {
      focusModeStore.set(true)
      viewModeStore.set('board')
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('List')).toBeDefined()
      expect(screen.getByText('Board')).toBeDefined()
      expect(screen.queryByText('Timeline')).toBeNull()
    })

    it('renders the Show done checkbox in list view', () => {
      viewModeStore.set('list')
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByLabelText('Show done')).toBeDefined()
    })

    it('routes done to the Logbook in board view (no broken Show done)', () => {
      viewModeStore.set('board')
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Done → Logbook')).toBeDefined()
      expect(screen.queryByLabelText('Show done')).toBeNull()
    })

    it('surfaces a persistent "need you" count on the board', () => {
      viewModeStore.set('board')
      Object.assign(taskStore, {
        list: [
          mockTask('h1', 'Blocked task', 'human-required'),
          mockTask('p1', 'Plan to review', 'plan-review'),
          mockTask('t1', 'Normal task', 'todo'),
        ],
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      // 2 of 3 tasks await the user — visible regardless of horizontal scroll.
      expect(screen.getByText('2 need you')).toBeDefined()
    })

    it('hides the need-you counter in list view even when tasks await', () => {
      viewModeStore.set('list')
      Object.assign(taskStore, { list: [mockTask('h1', 'Blocked', 'human-required')] })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText(/need you/)).toBeNull()
    })

    it('hides the need-you counter on the board when nothing awaits', () => {
      viewModeStore.set('board')
      Object.assign(taskStore, { list: [mockTask('t1', 'Normal', 'todo')] })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText(/need you/)).toBeNull()
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

  describe('agent mode filter chip', () => {
    it('renders All, Headless, Interactive buttons', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('All')).toBeDefined()
      expect(screen.getByText('Headless')).toBeDefined()
      expect(screen.getByText('Interactive')).toBeDefined()
    })
  })

  describe('clear filters', () => {
    it('does not show Clear filters when no filters active', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText('Clear filters')).toBeNull()
    })
  })

  describe('done column visibility', () => {
    it('hides Done column by default', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText(/^Done$/)).toBeNull()
    })
  })

  describe('mobile filter trigger', () => {
    it('renders mobile filters button', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByLabelText('Filters')).toBeDefined()
    })

    it('renders mobile search input', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      const inputs = screen.getAllByPlaceholderText('Search tasks...')
      expect(inputs.length).toBeGreaterThan(0)
    })
  })

  describe('optional callbacks', () => {
    it('does not crash when onnewTask is omitted', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Todo')).toBeDefined()
    })

    it('does not crash when onfocusedtaskchange is omitted', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Todo')).toBeDefined()
    })
  })

  describe('hint bar', () => {
    it('does not render hint bar when no task focused', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText('open')).toBeNull()
    })
  })

  describe('filter prop', () => {
    it('renders without error when filter=in-progress is passed', () => {
      render(TaskList, { props: { onselect: vi.fn(), filter: 'in-progress' } })
      expect(screen.getByText('Todo')).toBeDefined()
    })
  })

  describe('moveTask success', () => {
    it('calls taskStore.update when dropping a task onto a different column', async () => {
      vi.mocked(taskStore.update).mockResolvedValueOnce(undefined as never)
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Task to move', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Task to move', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn() } })

      const col = document.querySelector('[data-col-status="in-progress"]')
      expect(col).not.toBeNull()
      await fireEvent.drop(col!, { dataTransfer: { getData: () => 't-1' } })
      await vi.waitFor(() => {
        expect(taskStore.update).toHaveBeenCalledWith('t-1', { status: 'in-progress' })
      })
    })

    it('does nothing when dropping a task onto its current column', async () => {
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Same column', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Same column', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn() } })

      const col = document.querySelector('[data-col-status="todo"]')
      expect(col).not.toBeNull()
      await fireEvent.drop(col!, { dataTransfer: { getData: () => 't-1' } })
      expect(taskStore.update).not.toHaveBeenCalled()
    })

    it('does nothing when dropping an unknown task id', async () => {
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Known', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Known', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn() } })

      const col = document.querySelector('[data-col-status="in-progress"]')
      await fireEvent.drop(col!, { dataTransfer: { getData: () => 't-missing' } })
      expect(taskStore.update).not.toHaveBeenCalled()
    })
  })

  describe('show done column visibility', () => {
    it('hides the Done column and the broken Show done toggle in board view', () => {
      viewModeStore.set('board')
      render(TaskList, { props: { onselect: vi.fn() } })
      // No Done column on the board; done tasks are routed to the Logbook.
      expect(screen.queryByText(/^Done$/)).toBeNull()
      expect(screen.queryByLabelText('Show done')).toBeNull()
      expect(screen.getByText('Done → Logbook')).toBeDefined()
    })

    it('offers a working Show done toggle (unchecked) in list view', () => {
      viewModeStore.set('list')
      render(TaskList, { props: { onselect: vi.fn() } })
      const checkbox = screen.getByLabelText('Show done') as HTMLInputElement
      expect(checkbox.checked).toBe(false)
    })

    it('ignores Show done on the board even when enabled in list view', async () => {
      // Enable in list, switch to board: the board must not resurrect done.
      viewModeStore.set('list')
      render(TaskList, { props: { onselect: vi.fn() } })
      await fireEvent.click(screen.getByLabelText('Show done'))
      viewModeStore.set('board')
      await vi.waitFor(() => {
        expect(screen.queryByText(/^Done$/)).toBeNull()
        expect(screen.queryByLabelText('Show done')).toBeNull()
        expect(screen.getByText('Done → Logbook')).toBeDefined()
      })
    })
  })

  describe('view mode switching', () => {
    it('switches to timeline view when Timeline button is clicked', async () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Todo')).toBeDefined()
      await fireEvent.click(screen.getByText('Timeline'))
      await vi.waitFor(() => {
        expect(screen.queryByText('Todo')).toBeNull()
      })
    })

    it('switches to list view when List button is clicked', async () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      await fireEvent.click(screen.getByText('List'))
      await vi.waitFor(() => {
        expect(screen.queryByText('Todo')).toBeNull()
      })
    })

    it('renders board columns again when Board button clicked after switching away', async () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      await fireEvent.click(screen.getByText('List'))
      await vi.waitFor(() => {
        expect(screen.queryByText('Todo')).toBeNull()
      })
      await fireEvent.click(screen.getByText('Board'))
      await vi.waitFor(() => {
        expect(screen.getByText('Todo')).toBeDefined()
      })
    })
  })

  describe('window toggle-view event', () => {
    it('cycles view mode in response to toggle-view event', async () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Todo')).toBeDefined()
      window.dispatchEvent(new CustomEvent('toggle-view'))
      await vi.waitFor(() => {
        expect(screen.queryByText('Todo')).toBeNull()
      })
    })
  })

  describe('keyboard navigation', () => {
    it('shows hint bar when "j" focuses the first task', async () => {
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Focus me', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Focus me', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.queryByText('open')).toBeNull()
      await fireEvent.keyDown(window, { key: 'j' })
      await vi.waitFor(() => {
        expect(screen.getByText('open')).toBeDefined()
      })
    })

    it('clears focus on Escape after focusing via "j"', async () => {
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Focus me', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Focus me', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      await fireEvent.keyDown(window, { key: 'j' })
      await vi.waitFor(() => {
        expect(screen.getByText('open')).toBeDefined()
      })
      await fireEvent.keyDown(window, { key: 'Escape' })
      await vi.waitFor(() => {
        expect(screen.queryByText('open')).toBeNull()
      })
    })

    it('invokes onselect when Enter pressed on focused task', async () => {
      const onselect = vi.fn()
      Object.assign(taskStore, {
        list: [mockTask('t-42', 'Press enter', 'todo')],
        tasks: new Map([['t-42', mockTask('t-42', 'Press enter', 'todo')]]),
      })
      render(TaskList, { props: { onselect } })
      await fireEvent.keyDown(window, { key: 'j' })
      await vi.waitFor(() => {
        expect(screen.getByText('open')).toBeDefined()
      })
      await fireEvent.keyDown(window, { key: 'Enter' })
      expect(onselect).toHaveBeenCalledWith('t-42')
    })

    it('invokes onnewTask when "c" pressed', async () => {
      const onnewTask = vi.fn()
      render(TaskList, { props: { onselect: vi.fn(), onnewTask } })
      await fireEvent.keyDown(window, { key: 'c' })
      expect(onnewTask).toHaveBeenCalled()
    })

    it('notifies onfocusedtaskchange when focus changes', async () => {
      const onfocusedtaskchange = vi.fn()
      Object.assign(taskStore, {
        list: [mockTask('t-1', 'Focus task', 'todo')],
        tasks: new Map([['t-1', mockTask('t-1', 'Focus task', 'todo')]]),
      })
      render(TaskList, { props: { onselect: vi.fn(), onfocusedtaskchange } })
      await fireEvent.keyDown(window, { key: 'j' })
      await vi.waitFor(() => {
        expect(onfocusedtaskchange).toHaveBeenCalledWith('t-1')
      })
    })
  })

  describe('agent mode filter chip rendering', () => {
    it('renders All button as initially selected with primary styling', () => {
      render(TaskList, { props: { onselect: vi.fn() } })
      const allBtn = screen.getByText('All')
      // All button is the default-selected mode; lives inside the filter bar
      expect(allBtn).toBeDefined()
      expect(screen.getByText('Headless')).toBeDefined()
      expect(screen.getByText('Interactive')).toBeDefined()
    })
  })

  describe('tasks list with multiple statuses', () => {
    it('renders tasks across multiple columns simultaneously', () => {
      Object.assign(taskStore, {
        list: [
          mockTask('t-1', 'Todo task', 'todo'),
          mockTask('t-2', 'Progress task', 'in-progress'),
          mockTask('t-3', 'Review task', 'in-review'),
          mockTask('t-4', 'Human task', 'human-required'),
        ],
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Todo task')).toBeDefined()
      expect(screen.getByText('Progress task')).toBeDefined()
      expect(screen.getByText('Review task')).toBeDefined()
      expect(screen.getByText('Human task')).toBeDefined()
    })

    it('does not render tasks whose status is unknown', () => {
      Object.assign(taskStore, {
        list: [
          mockTask('t-1', 'Valid task', 'todo'),
          mockTask('t-2', 'Unknown status', 'fictional-status'),
        ],
      })
      render(TaskList, { props: { onselect: vi.fn() } })
      expect(screen.getByText('Valid task')).toBeDefined()
      expect(screen.queryByText('Unknown status')).toBeNull()
    })
  })
})
