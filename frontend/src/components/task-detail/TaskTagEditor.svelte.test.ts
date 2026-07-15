import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockUpdate = vi.fn()
vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: { update: (...args: unknown[]) => mockUpdate(...args) },
}))

const TaskTagEditor = (await import('./TaskTagEditor.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'X',
  status: 'todo',
  body: '',
  tags: ['backend'],
  agentMode: 'headless',
  allowedTools: [],
  projectId: 'foo/bar',
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  dueDate: null,
}

describe('TaskTagEditor', () => {
  beforeEach(() => {
    mockUpdate.mockReset()
    mockUpdate.mockResolvedValue(baseTask)
  })
  afterEach(cleanup)

  it('renders existing tags in display mode', () => {
    render(TaskTagEditor, { props: { task: baseTask as never } })
    expect(screen.getByText('backend')).toBeDefined()
  })

  it('renders placeholder when no tags', () => {
    render(TaskTagEditor, { props: { task: { ...baseTask, tags: [] } as never } })
    expect(screen.getByText('add tags')).toBeDefined()
  })

  it('click switches to edit mode and shows input', async () => {
    render(TaskTagEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('backend'))
    await waitFor(() => {
      expect(screen.getAllByRole('textbox').length).toBeGreaterThan(0)
    })
  })

  it('Enter on non-empty input adds tag', async () => {
    render(TaskTagEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('backend'))
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'frontend' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(screen.getByText('frontend')).toBeDefined()
    })
  })

  it('Enter on empty input persists and exits edit mode', async () => {
    render(TaskTagEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('backend'))
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'frontend' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    // Now add a second tag and persist
    await fireEvent.input(input, { target: { value: '' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('t1', { tags: ['backend', 'frontend'] })
    })
  })

  it('Escape cancels without calling update', async () => {
    render(TaskTagEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('backend'))
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'frontend' } })
    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('Backspace on empty input pops last tag', async () => {
    const task = { ...baseTask, tags: ['a', 'b'] }
    render(TaskTagEditor, { props: { task: task as never } })
    await fireEvent.click(screen.getByText('a'))
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    await fireEvent.keyDown(input, { key: 'Backspace' })
    // 'b' should be gone from the editor draft
    await waitFor(() => {
      expect(screen.queryByText('b')).toBeNull()
    })
  })

  it('comma key adds tag without submitting', async () => {
    render(TaskTagEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('backend'))
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'devops' } })
    await fireEvent.keyDown(input, { key: ',' })
    await waitFor(() => {
      expect(screen.getByText('devops')).toBeDefined()
    })
    expect(mockUpdate).not.toHaveBeenCalled()
  })
})
