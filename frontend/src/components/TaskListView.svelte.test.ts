import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const TaskListView = (await import('./TaskListView.svelte')).default

const tasks = [
  {
    id: 't1', title: 'First', status: 'todo', priority: 'high',
    projectId: 'foo/bar', updatedAt: '2026-04-01T00:00:00Z', tags: [], agentMode: 'headless',
  },
  {
    id: 't2', title: 'Second', status: 'in-progress', priority: 'low',
    projectId: '', updatedAt: '2026-04-02T00:00:00Z', tags: [], agentMode: 'headless',
  },
]

describe('TaskListView', () => {
  afterEach(cleanup)

  it('renders rows for each task', () => {
    render(TaskListView, {
      props: {
        tasks: tasks as never,
        focusedTaskId: null,
        onselect: vi.fn(),
        onhover: vi.fn(),
      },
    })
    expect(screen.getByText('First')).toBeDefined()
    expect(screen.getByText('Second')).toBeDefined()
  })

  it('shows project for tasks that have one, em-dash otherwise', () => {
    render(TaskListView, {
      props: {
        tasks: tasks as never,
        focusedTaskId: null,
        onselect: vi.fn(),
        onhover: vi.fn(),
      },
    })
    expect(screen.getByText('foo/bar')).toBeDefined()
    expect(screen.getByText('—')).toBeDefined()
  })

  it('row click fires onselect with task id', async () => {
    const onselect = vi.fn()
    render(TaskListView, {
      props: {
        tasks: tasks as never,
        focusedTaskId: null,
        onselect,
        onhover: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByText('First'))
    expect(onselect).toHaveBeenCalledWith('t1')
  })

  it('mouseenter fires onhover with row index', async () => {
    const onhover = vi.fn()
    render(TaskListView, {
      props: {
        tasks: tasks as never,
        focusedTaskId: null,
        onselect: vi.fn(),
        onhover,
      },
    })
    await fireEvent.mouseEnter(screen.getByText('Second').closest('tr')!)
    expect(onhover).toHaveBeenCalledWith(1)
  })

  it('empty state renders "No tasks match your filters"', () => {
    render(TaskListView, {
      props: {
        tasks: [],
        focusedTaskId: null,
        onselect: vi.fn(),
        onhover: vi.fn(),
      },
    })
    expect(screen.getByText('No tasks match your filters')).toBeDefined()
  })

  it('focused row gets primary-tinted background', () => {
    const { container } = render(TaskListView, {
      props: {
        tasks: tasks as never,
        focusedTaskId: 't2',
        onselect: vi.fn(),
        onhover: vi.fn(),
      },
    })
    expect(container.querySelector('[data-focused-task]')).toBeTruthy()
  })
})
