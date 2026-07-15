import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockUpdate = vi.fn()
vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: { update: (...args: unknown[]) => mockUpdate(...args) },
}))

const TaskDueDateEditor = (await import('./TaskDueDateEditor.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'X',
  status: 'todo',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  projectId: 'foo/bar',
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  dueDate: null,
}

describe('TaskDueDateEditor', () => {
  beforeEach(() => {
    mockUpdate.mockReset()
    mockUpdate.mockResolvedValue(baseTask)
  })
  afterEach(cleanup)

  it('renders display state when no due date', () => {
    render(TaskDueDateEditor, { props: { task: baseTask as never } })
    // formatDueDateDisplay(null) returns the empty-state label
    expect(screen.getByRole('button')).toBeDefined()
  })

  it('clicking display switches to edit input', async () => {
    render(TaskDueDateEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByRole('button'))
    expect(await screen.findByPlaceholderText('today / tomorrow / YYYY-MM-DD')).toBeDefined()
  })

  it('Enter with "today" saves an ISO date', async () => {
    render(TaskDueDateEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('today / tomorrow / YYYY-MM-DD')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'today' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalled()
      const args = mockUpdate.mock.calls[0]
      expect(args[0]).toBe('t1')
      expect(args[1].due_date).toBeTruthy()
    })
  })

  it('invalid date reports via onerror, does not call update', async () => {
    const errors: string[] = []
    render(TaskDueDateEditor, {
      props: {
        task: baseTask as never,
        onerror: (m: string) => { errors.push(m) },
      },
    })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('today / tomorrow / YYYY-MM-DD')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'invalid garbage' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(errors.some(e => /Invalid date/.test(e))).toBe(true)
    })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('Escape cancels without saving', async () => {
    render(TaskDueDateEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('today / tomorrow / YYYY-MM-DD')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'today' } })
    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('"clear" sets due_date to null', async () => {
    const taskWithDate = { ...baseTask, dueDate: '2026-12-01T00:00:00Z' }
    render(TaskDueDateEditor, { props: { task: taskWithDate as never } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('today / tomorrow / YYYY-MM-DD')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'clear' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('t1', { due_date: null })
    })
  })
})
