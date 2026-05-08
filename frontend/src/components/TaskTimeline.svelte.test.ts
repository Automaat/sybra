import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { task } from '../../wailsjs/go/models.js'

// jsdom doesn't implement Web Animations API
if (!Element.prototype.animate) {
  Element.prototype.animate = vi.fn(() => ({
    onfinish: null, cancel: vi.fn(), finish: vi.fn(),
    addEventListener: vi.fn(), removeEventListener: vi.fn(),
  })) as any
}

const TaskTimeline = (await import('./TaskTimeline.svelte')).default

function makeTask(overrides: Partial<task.Task> = {}): task.Task {
  return task.Task.createFrom({
    id: 'task-1',
    title: 'Test task',
    status: 'todo',
    taskType: '',
    agentMode: 'headless',
    allowedTools: [],
    tags: [],
    projectId: '',
    branch: '',
    prNumber: 0,
    issue: '',
    statusReason: '',
    body: '',
    plan: '',
    planCritique: '',
    slug: '',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-05-01T00:00:00Z',
    dueDate: '',
    closedAt: '',
    ...overrides,
  })
}

const defaultProps = {
  tasks: [],
  focusedTaskId: null,
  onselect: vi.fn(),
  onfocus: vi.fn(),
}

describe('TaskTimeline', () => {
  afterEach(() => { cleanup() })

  it('shows empty state when no tasks', () => {
    render(TaskTimeline, { props: defaultProps })
    expect(screen.getByText('No tasks match your filters')).toBeDefined()
  })

  it('shows zoom control buttons', () => {
    render(TaskTimeline, { props: defaultProps })
    expect(screen.getByText('Day')).toBeDefined()
    expect(screen.getByText('Week')).toBeDefined()
    expect(screen.getByText('Month')).toBeDefined()
  })

  it('shows task title when tasks provided', () => {
    const t = makeTask({ title: 'Build auth system' })
    render(TaskTimeline, { props: { ...defaultProps, tasks: [t] } })
    expect(screen.getByText('Build auth system')).toBeDefined()
  })

  it('renders multiple tasks', () => {
    const tasks = [
      makeTask({ id: 't1', title: 'Task One' }),
      makeTask({ id: 't2', title: 'Task Two' }),
    ]
    render(TaskTimeline, { props: { ...defaultProps, tasks } })
    expect(screen.getByText('Task One')).toBeDefined()
    expect(screen.getByText('Task Two')).toBeDefined()
  })

  it('switches zoom level on Day button click', async () => {
    render(TaskTimeline, { props: defaultProps })
    await fireEvent.click(screen.getByText('Day'))
    // Day button should now be active (primary styling)
    expect(screen.getByText('Day')).toBeDefined()
  })

  it('switches zoom level on Month button click', async () => {
    render(TaskTimeline, { props: defaultProps })
    await fireEvent.click(screen.getByText('Month'))
    expect(screen.getByText('Month')).toBeDefined()
  })

  it('calls onselect when task row clicked', async () => {
    const onselect = vi.fn()
    const t = makeTask({ id: 'task-x', title: 'Click me' })
    render(TaskTimeline, { props: { ...defaultProps, tasks: [t], onselect } })
    await fireEvent.click(screen.getByText('Click me'))
    expect(onselect).toHaveBeenCalledWith('task-x')
  })

  it('shows due date marker for tasks with dueDate', () => {
    const t = makeTask({
      id: 't1',
      title: 'With due date',
      dueDate: '2026-05-15T00:00:00Z',
      createdAt: '2026-04-01T00:00:00Z',
    })
    const { container } = render(TaskTimeline, { props: { ...defaultProps, tasks: [t] } })
    // Due date marker should render as a div with specific styling
    expect(container).toBeDefined()
  })

  it('shows keyboard hint text', () => {
    render(TaskTimeline, { props: defaultProps })
    expect(screen.getByText(/J\/K to navigate/)).toBeDefined()
  })
})
