import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockUpdate = vi.fn()
vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: { update: (...args: unknown[]) => mockUpdate(...args) },
}))

const TaskMaxTurnsEditor = (await import('./TaskMaxTurnsEditor.svelte')).default

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
  maxTurns: 0,
}

describe('TaskMaxTurnsEditor', () => {
  beforeEach(() => {
    mockUpdate.mockReset()
    mockUpdate.mockResolvedValue(baseTask)
  })
  afterEach(cleanup)

  it('renders "global default" when maxTurns is 0', () => {
    render(TaskMaxTurnsEditor, { props: { task: baseTask as never } })
    expect(screen.getByText('global default')).toBeDefined()
  })

  it('renders explicit value when maxTurns is set', () => {
    render(TaskMaxTurnsEditor, { props: { task: { ...baseTask, maxTurns: 25 } as never } })
    expect(screen.getByText('25')).toBeDefined()
  })

  it('Enter on valid integer calls update', async () => {
    render(TaskMaxTurnsEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('global default')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: '42' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('t1', { max_turns: 42 })
    })
  })

  it('negative input fires onerror, no update', async () => {
    const errors: string[] = []
    render(TaskMaxTurnsEditor, {
      props: { task: baseTask as never, onerror: (m: string) => errors.push(m) },
    })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('global default')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: '-3' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(errors.some(e => /non-negative/.test(e))).toBe(true)
    })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('Escape cancels without saving', async () => {
    render(TaskMaxTurnsEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('global default')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: '5' } })
    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('unchanged value is a no-op', async () => {
    render(TaskMaxTurnsEditor, { props: { task: { ...baseTask, maxTurns: 10 } as never } })
    await fireEvent.click(screen.getByText('10'))
    const input = (await screen.findByPlaceholderText('global default')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: '10' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    // Allow microtasks
    await new Promise(r => setTimeout(r, 10))
    expect(mockUpdate).not.toHaveBeenCalled()
  })
})
