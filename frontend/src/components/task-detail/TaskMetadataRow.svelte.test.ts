import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockUpdate = vi.fn()
const mockOpenURL = vi.fn()
const mockPushLocal = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    update: (...args: unknown[]) => mockUpdate(...args),
  },
}))

vi.mock('../../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: (...args: unknown[]) => mockPushLocal(...args),
  },
}))

vi.mock('$lib/browser.svelte.js', () => ({
  openLink: (...args: unknown[]) => mockOpenURL(...args),
}))

const TaskMetadataRow = (await import('./TaskMetadataRow.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'X',
  status: 'todo',
  body: '',
  tags: ['backend'],
  agentMode: 'headless',
  allowedTools: ['Read', 'Bash'],
  projectId: 'foo/bar',
  issue: 'https://example/i/1',
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  dueDate: null,
}

describe('TaskMetadataRow', () => {
  beforeEach(() => {
    mockUpdate.mockReset()
    mockOpenURL.mockReset()
    mockPushLocal.mockReset()
    mockUpdate.mockResolvedValue(baseTask)
  })
  afterEach(cleanup)

  it('keeps the per-run knobs editable even at their default value', () => {
    // They render quietly at default (muted "disabled"/"global default") but
    // stay present — they are the only place to set max turns / fork.
    render(TaskMetadataRow, { props: { task: baseTask as never } })
    expect(screen.getByText('Fork Subagents')).toBeDefined()
    expect(screen.getByText('disabled')).toBeDefined()
    expect(screen.getByText('Max Turns')).toBeDefined()
    expect(screen.getByText('global default')).toBeDefined()
  })

  it('renders agent mode + tags + project + branch + issue + allowed tools', () => {
    render(TaskMetadataRow, { props: { task: baseTask as never } })
    expect(screen.getByText('Agent Mode')).toBeDefined()
    expect(screen.getByText('headless')).toBeDefined()
    expect(screen.getByText('backend')).toBeDefined()
    expect(screen.getByText('foo/bar')).toBeDefined()
    expect(screen.getByText(/sybra\/demo-t1/)).toBeDefined()
    expect(screen.getByText('https://example/i/1')).toBeDefined()
    expect(screen.getByText('Read')).toBeDefined()
    expect(screen.getByText('Bash')).toBeDefined()
  })

  it('starts editing tags via window event and adds a new tag', async () => {
    render(TaskMetadataRow, { props: { task: baseTask as never } })
    window.dispatchEvent(new CustomEvent('task-detail:edit-tags'))
    await waitFor(() => {
      expect(screen.queryAllByRole('textbox').length).toBeGreaterThan(0)
    })
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'frontend' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(screen.getByText('frontend')).toBeDefined()
    })
  })

  it('saves due date "today" via Enter', async () => {
    render(TaskMetadataRow, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('Set due date'))
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

  it('shows error on invalid due date', async () => {
    render(TaskMetadataRow, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('Set due date'))
    const input = (await screen.findByPlaceholderText('today / tomorrow / YYYY-MM-DD')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'invalid garbage' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(screen.getByText(/Invalid date/)).toBeDefined()
    })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('opens issue url on click', async () => {
    render(TaskMetadataRow, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('https://example/i/1'))
    expect(mockOpenURL).toHaveBeenCalledWith('https://example/i/1', expect.anything())
  })
})
