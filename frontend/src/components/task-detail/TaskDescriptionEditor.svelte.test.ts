import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockUpdate = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    update: (...args: unknown[]) => mockUpdate(...args),
  },
}))

vi.mock('../../lib/markdown.js', () => ({
  renderMarkdown: (s: unknown) => (s ? `<p>${s}</p>` : ''),
  renderChecklistMarkdown: (s: unknown) => (s ? `<p>${s}</p>` : ''),
}))

const TaskDescriptionEditor = (await import('./TaskDescriptionEditor.svelte')).default

const baseTask = {
  id: 't1',
  title: 'X',
  status: 'todo',
  body: 'initial body',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('TaskDescriptionEditor', () => {
  beforeEach(() => {
    mockUpdate.mockReset()
    mockUpdate.mockResolvedValue(baseTask)
  })
  afterEach(cleanup)

  it('renders body markdown by default', () => {
    render(TaskDescriptionEditor, { props: { task: baseTask as never } })
    expect(screen.getByText('Description')).toBeDefined()
    expect(screen.getByText('initial body')).toBeDefined()
  })

  it('shows placeholder when body empty', () => {
    render(TaskDescriptionEditor, {
      props: { task: { ...baseTask, body: '' } as never },
    })
    expect(screen.getByText('Click to add description...')).toBeDefined()
  })

  it('opens textarea on click and saves with Cmd+Enter', async () => {
    render(TaskDescriptionEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('initial body'))
    const ta = (await screen.findByDisplayValue('initial body')) as HTMLTextAreaElement
    await fireEvent.input(ta, { target: { value: 'updated body' } })
    await fireEvent.keyDown(ta, { key: 'Enter', metaKey: true })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('t1', { body: 'updated body' })
    })
  })

  it('cancels on Escape without saving', async () => {
    render(TaskDescriptionEditor, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('initial body'))
    const ta = (await screen.findByDisplayValue('initial body')) as HTMLTextAreaElement
    await fireEvent.input(ta, { target: { value: 'X' } })
    await fireEvent.keyDown(ta, { key: 'Escape' })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('renders plan and code-review when present', () => {
    render(TaskDescriptionEditor, {
      props: {
        task: {
          ...baseTask,
          plan: 'plan body',
          codeReview: 'review body',
        } as never,
      },
    })
    expect(screen.getByText('Plan')).toBeDefined()
    expect(screen.getByText('Code Review (auto-generated)')).toBeDefined()
  })

  it('opens editor when task-detail:edit-body event fires', async () => {
    render(TaskDescriptionEditor, { props: { task: baseTask as never } })
    window.dispatchEvent(new CustomEvent('task-detail:edit-body'))
    await waitFor(() => {
      expect(screen.getByDisplayValue('initial body')).toBeDefined()
    })
  })
})
