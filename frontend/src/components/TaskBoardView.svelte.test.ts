import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: { create: vi.fn(), update: vi.fn() },
}))
vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: { pushLocal: vi.fn() },
}))

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
  afterEach(cleanup)

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
