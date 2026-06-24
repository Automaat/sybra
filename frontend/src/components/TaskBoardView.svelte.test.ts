import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: { create: vi.fn(), update: vi.fn() },
}))
vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: { pushLocal: vi.fn() },
}))

const agentMock = vi.hoisted(() => ({ list: [] as Array<Record<string, unknown>> }))
vi.mock('../stores/agents.svelte.js', () => ({ agentStore: agentMock }))

// Drives the empty-column thin-rail (desktop only); default off keeps the
// existing populated-column tests on the normal layout.
const vpMock = vi.hoisted(() => ({ isDesktop: false }))
vi.mock('../lib/viewport.svelte.js', () => ({ viewport: vpMock }))

const TaskBoardView = (await import('./TaskBoardView.svelte')).default

const columns = [
  { status: 'todo', label: 'Todo', border: 'border-t-surface-400', includes: [] },
  { status: 'in-progress', label: 'In Progress', border: 'border-t-primary-500', includes: [] },
]

const tasks = [
  { id: 't1', title: 'First', status: 'todo', priority: 'high', tags: [], agentMode: 'headless' },
  { id: 't2', title: 'Second', status: 'in-progress', priority: 'low', tags: [], agentMode: 'headless' },
]

function columnTasks(col: { status: string }) {
  return tasks.filter(t => t.status === col.status) as never[]
}

describe('TaskBoardView', () => {
  afterEach(() => {
    cleanup()
    vpMock.isDesktop = false
    agentMock.list = []
  })

  it('shows a static running-agent count in the column header', () => {
    agentMock.list = [{ taskId: 't2', state: 'running', name: '' }]
    render(TaskBoardView, {
      props: {
        visibleColumns: columns as never,
        columnTasks: columnTasks as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove: vi.fn(),
        ontogglecolumn: vi.fn(),
      },
    })
    // t2 lives in the In Progress column and has a running agent.
    expect(screen.getByTitle('1 agent(s) working in this column')).toBeDefined()
  })

  it('collapses an empty desktop column to a thin rail, expandable on click', async () => {
    vpMock.isDesktop = true
    const cols = [
      { status: 'todo', label: 'Todo', border: '', includes: [] },
      { status: 'testing', label: 'Testing', border: '', includes: [] },
    ]
    // todo has a task; testing is empty → thin rail.
    const ct = (col: { status: string }) =>
      (col.status === 'todo' ? [tasks[0]] : []) as never[]
    render(TaskBoardView, {
      props: {
        visibleColumns: cols as never,
        columnTasks: ct as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove: vi.fn(),
        ontogglecolumn: vi.fn(),
      },
    })
    const rail = screen.getByTitle(/Testing \(empty\)/)
    expect(rail.textContent).toContain('0')
    await fireEvent.click(rail)
    // Expanding replaces the thin rail with the normal column.
    expect(screen.queryByTitle(/Testing \(empty\)/)).toBeNull()
  })

  it('drops a task onto an empty thin rail and fires onmove', async () => {
    vpMock.isDesktop = true
    const onmove = vi.fn()
    const cols = [
      { status: 'todo', label: 'Todo', border: '', includes: [] },
      { status: 'testing', label: 'Testing', border: '', includes: [] },
    ]
    const ct = (col: { status: string }) =>
      (col.status === 'todo' ? [tasks[0]] : []) as never[]
    render(TaskBoardView, {
      props: {
        visibleColumns: cols as never,
        columnTasks: ct as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove,
        ontogglecolumn: vi.fn(),
      },
    })
    const rail = screen.getByTitle(/Testing \(empty\)/)
    const dataTransfer = { getData: () => 't1' }
    await fireEvent.drop(rail, { dataTransfer })
    expect(onmove).toHaveBeenCalledWith('t1', 'testing')
  })

  it('renders one column per visibleColumns entry', () => {
    render(TaskBoardView, {
      props: {
        visibleColumns: columns as never,
        columnTasks: columnTasks as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove: vi.fn(),
        ontogglecolumn: vi.fn(),
      },
    })
    expect(screen.getByRole('heading', { name: 'Todo' })).toBeDefined()
    expect(screen.getByRole('heading', { name: 'In Progress' })).toBeDefined()
  })

  it('renders task cards inside their column', () => {
    render(TaskBoardView, {
      props: {
        visibleColumns: columns as never,
        columnTasks: columnTasks as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove: vi.fn(),
        ontogglecolumn: vi.fn(),
      },
    })
    expect(screen.getByText('First')).toBeDefined()
    expect(screen.getByText('Second')).toBeDefined()
  })

  it('column header click fires ontogglecolumn with the status', async () => {
    const ontogglecolumn = vi.fn()
    render(TaskBoardView, {
      props: {
        visibleColumns: columns as never,
        columnTasks: columnTasks as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove: vi.fn(),
        ontogglecolumn,
      },
    })
    await fireEvent.click(screen.getByRole('heading', { name: 'Todo' }))
    expect(ontogglecolumn).toHaveBeenCalledWith('todo')
  })

  it('drop on column fires onmove with task id + target status', async () => {
    const onmove = vi.fn()
    const { container } = render(TaskBoardView, {
      props: {
        visibleColumns: columns as never,
        columnTasks: columnTasks as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove,
        ontogglecolumn: vi.fn(),
      },
    })
    const dropZone = container.querySelector('[data-col-status="in-progress"]') as HTMLElement
    const dataTransfer = { getData: (k: string) => (k === 'text/plain' ? 't1' : '') }
    await fireEvent.drop(dropZone, { dataTransfer })
    expect(onmove).toHaveBeenCalledWith('t1', 'in-progress')
  })

  it('count badge shows column task count', () => {
    render(TaskBoardView, {
      props: {
        visibleColumns: columns as never,
        columnTasks: columnTasks as never,
        focusedTaskId: null,
        collapsedColumns: new Set<string>(),
        onselect: vi.fn(),
        onmove: vi.fn(),
        ontogglecolumn: vi.fn(),
      },
    })
    // Each column has exactly one task; render two "1" badges
    const ones = screen.getAllByText('1')
    expect(ones.length).toBe(2)
  })
})
