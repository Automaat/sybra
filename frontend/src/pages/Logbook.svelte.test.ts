import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    loading: false,
    list: [],
  },
}))

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    list: [],
  },
}))

const { taskStore } = await import('../stores/tasks.svelte.js')
const { projectStore } = await import('../stores/projects.svelte.js')
const Logbook = (await import('./Logbook.svelte')).default

function makeTask(overrides: Record<string, unknown> = {}) {
  return {
    id: 'task-1',
    title: 'Completed task',
    status: 'done',
    agentMode: 'headless',
    allowedTools: [],
    tags: [],
    projectId: '',
    body: '',
    closedAt: '2026-04-01T10:00:00Z',
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  }
}

describe('Logbook', () => {
  beforeEach(() => {
    Object.assign(taskStore, { loading: false, list: [] })
    Object.assign(projectStore, { list: [] })
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  describe('loading and empty states', () => {
    it('shows loading message when taskStore.loading is true', () => {
      Object.assign(taskStore, { loading: true, list: [] })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('Loading…')).toBeDefined()
    })

    it('shows empty logbook message when no done/cancelled tasks', () => {
      Object.assign(taskStore, {
        list: [makeTask({ status: 'todo' }), makeTask({ status: 'in-progress' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('Nothing in the logbook yet')).toBeDefined()
    })

    it('shows "N closed" count in header', () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', status: 'done' }), makeTask({ id: 't2', status: 'cancelled' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('2 closed')).toBeDefined()
    })

    it('excludes non-terminal tasks from count', () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', status: 'done' }),
          makeTask({ id: 't2', status: 'todo' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('1 closed')).toBeDefined()
    })
  })

  describe('task display', () => {
    it('renders task titles in the table', () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', title: 'My finished task', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('My finished task')).toBeDefined()
    })

    it('renders both done and cancelled tasks', () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Done task', status: 'done' }),
          makeTask({ id: 't2', title: 'Cancelled task', status: 'cancelled' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('Done task')).toBeDefined()
      expect(screen.getByText('Cancelled task')).toBeDefined()
    })

    it('calls onviewtask when a task row is clicked', async () => {
      const onviewtask = vi.fn()
      Object.assign(taskStore, {
        list: [makeTask({ id: 'task-abc', title: 'Clickable task', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask } })
      await fireEvent.click(screen.getByText('Clickable task'))
      expect(onviewtask).toHaveBeenCalledWith('task-abc')
    })

    it('renders tags for tasks', () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', status: 'done', tags: ['backend', 'api'] })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const backendEls = screen.getAllByText('backend')
      expect(backendEls.length).toBeGreaterThan(0)
      const apiEls = screen.getAllByText('api')
      expect(apiEls.length).toBeGreaterThan(0)
    })

    it('shows project filter dropdown when projects exist', () => {
      Object.assign(projectStore, { list: [{ id: 'p1', owner: 'org', repo: 'myrepo' }] })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('All projects')).toBeDefined()
      expect(screen.getByText('org/myrepo')).toBeDefined()
    })

    it('hides project filter when no projects exist', () => {
      Object.assign(projectStore, { list: [] })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.queryByText('All projects')).toBeNull()
    })
  })

  describe('status filter pills', () => {
    it('renders All, Done, Cancelled pills', () => {
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByText('All')).toBeDefined()
      expect(screen.getByText('Done')).toBeDefined()
      expect(screen.getByText('Cancelled')).toBeDefined()
    })

    it('filters to only done tasks when Done pill clicked', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Done task', status: 'done' }),
          makeTask({ id: 't2', title: 'Cancelled task', status: 'cancelled' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      // The 'Done' pill button appears before the status badge in the DOM
      await fireEvent.click(screen.getAllByText('Done')[0])
      expect(screen.getByText('Done task')).toBeDefined()
      expect(screen.queryByText('Cancelled task')).toBeNull()
    })

    it('filters to only cancelled tasks when Cancelled pill clicked', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Done task', status: 'done' }),
          makeTask({ id: 't2', title: 'Cancelled task', status: 'cancelled' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      // The 'Cancelled' pill button appears before the status badge in the DOM
      await fireEvent.click(screen.getAllByText('Cancelled')[0])
      expect(screen.getByText('Cancelled task')).toBeDefined()
      expect(screen.queryByText('Done task')).toBeNull()
    })

    it('shows all tasks after clicking All again', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Done task', status: 'done' }),
          makeTask({ id: 't2', title: 'Cancelled task', status: 'cancelled' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      await fireEvent.click(screen.getAllByText('Done')[0])
      await fireEvent.click(screen.getByText('All'))
      expect(screen.getByText('Done task')).toBeDefined()
      expect(screen.getByText('Cancelled task')).toBeDefined()
    })
  })

  describe('search filtering', () => {
    it('filters tasks by title search', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Auth middleware fix', status: 'done' }),
          makeTask({ id: 't2', title: 'UI cleanup', status: 'done' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const searchInput = screen.getByPlaceholderText('Search…')
      await fireEvent.input(searchInput, { target: { value: 'auth' } })
      expect(screen.getByText('Auth middleware fix')).toBeDefined()
      expect(screen.queryByText('UI cleanup')).toBeNull()
    })

    it('filters tasks by body search', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Task A', body: 'contains postgres stuff', status: 'done' }),
          makeTask({ id: 't2', title: 'Task B', body: 'frontend code', status: 'done' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const searchInput = screen.getByPlaceholderText('Search…')
      await fireEvent.input(searchInput, { target: { value: 'postgres' } })
      expect(screen.getByText('Task A')).toBeDefined()
      expect(screen.queryByText('Task B')).toBeNull()
    })

    it('search is case-insensitive', async () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', title: 'Fix DATABASE issue', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const searchInput = screen.getByPlaceholderText('Search…')
      await fireEvent.input(searchInput, { target: { value: 'database' } })
      expect(screen.getByText('Fix DATABASE issue')).toBeDefined()
    })

    it('shows "No tasks match these filters" when search has no results', async () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', title: 'Auth fix', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const searchInput = screen.getByPlaceholderText('Search…')
      await fireEvent.input(searchInput, { target: { value: 'xyz-nonexistent' } })
      expect(screen.getByText('No tasks match these filters')).toBeDefined()
    })
  })

  describe('tag filtering', () => {
    it('renders tag pills from logbook tasks', () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', status: 'done', tags: ['backend', 'api'] })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const tagButtons = screen.getAllByText('backend')
      expect(tagButtons.length).toBeGreaterThan(0)
    })

    it('filters tasks by selected tag', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Backend task', status: 'done', tags: ['backend'] }),
          makeTask({ id: 't2', title: 'Frontend task', status: 'done', tags: ['frontend'] }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const tagPills = screen.getAllByText('backend')
      await fireEvent.click(tagPills[0])
      expect(screen.getByText('Backend task')).toBeDefined()
      expect(screen.queryByText('Frontend task')).toBeNull()
    })

    it('does not show tag pills from non-logbook tasks', () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', status: 'done', tags: ['done-tag'] }),
          makeTask({ id: 't2', status: 'todo', tags: ['todo-tag'] }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.queryByText('todo-tag')).toBeNull()
    })
  })

  describe('clear filters', () => {
    it('shows Clear button when search is active', async () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      await fireEvent.input(screen.getByPlaceholderText('Search…'), { target: { value: 'test' } })
      expect(screen.getByText('Clear')).toBeDefined()
    })

    it('does not show Clear button with no active filters', () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.queryByText('Clear')).toBeNull()
    })

    it('clears search when Clear button clicked', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Auth fix', status: 'done' }),
          makeTask({ id: 't2', title: 'UI task', status: 'done' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      await fireEvent.input(screen.getByPlaceholderText('Search…'), { target: { value: 'auth' } })
      expect(screen.queryByText('UI task')).toBeNull()
      await fireEvent.click(screen.getByText('Clear'))
      expect(screen.getByText('UI task')).toBeDefined()
    })

    it('clears filters from "No tasks match" link', async () => {
      Object.assign(taskStore, {
        list: [makeTask({ id: 't1', title: 'Auth fix', status: 'done' })],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      await fireEvent.input(screen.getByPlaceholderText('Search…'), { target: { value: 'xyz' } })
      expect(screen.getByText('No tasks match these filters')).toBeDefined()
      await fireEvent.click(screen.getByText('Clear filters'))
      expect(screen.getByText('Auth fix')).toBeDefined()
    })
  })

  describe('sort toggle', () => {
    it('shows "↓ Newest" sort button by default', () => {
      render(Logbook, { props: { onviewtask: vi.fn() } })
      expect(screen.getByTitle('Sort by closed date')).toBeDefined()
      expect(screen.getByText('↓ Newest')).toBeDefined()
    })

    it('toggles to "↑ Oldest" when sort button clicked', async () => {
      render(Logbook, { props: { onviewtask: vi.fn() } })
      await fireEvent.click(screen.getByTitle('Sort by closed date'))
      expect(screen.getByText('↑ Oldest')).toBeDefined()
    })

    it('sorts newest first by default (descending closedAt)', () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Older task', status: 'done', closedAt: '2026-01-01T00:00:00Z' }),
          makeTask({ id: 't2', title: 'Newer task', status: 'done', closedAt: '2026-03-01T00:00:00Z' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      const rows = screen.getAllByRole('row')
      const titles = rows.slice(1).map(r => r.querySelector('td')?.textContent)
      expect(titles[0]).toBe('Newer task')
      expect(titles[1]).toBe('Older task')
    })

    it('sorts oldest first when ascending toggled', async () => {
      Object.assign(taskStore, {
        list: [
          makeTask({ id: 't1', title: 'Older task', status: 'done', closedAt: '2026-01-01T00:00:00Z' }),
          makeTask({ id: 't2', title: 'Newer task', status: 'done', closedAt: '2026-03-01T00:00:00Z' }),
        ],
      })
      render(Logbook, { props: { onviewtask: vi.fn() } })
      await fireEvent.click(screen.getByTitle('Sort by closed date'))
      const rows = screen.getAllByRole('row')
      const titles = rows.slice(1).map(r => r.querySelector('td')?.textContent)
      expect(titles[0]).toBe('Older task')
      expect(titles[1]).toBe('Newer task')
    })
  })
})
