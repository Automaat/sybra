import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'

const mockCreate = vi.fn()
const mockUpdate = vi.fn()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    create: (...args: unknown[]) => mockCreate(...args),
    update: (...args: unknown[]) => mockUpdate(...args),
  },
}))

vi.mock('../stores/projects.svelte.js', () => ({
  projectStore: {
    list: [],
  },
}))

vi.mock('../lib/detectProject.js', () => ({
  detectProject: vi.fn().mockReturnValue(null),
}))

const QuickAddTask = (await import('./QuickAddTask.svelte')).default

describe('QuickAddTask', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    cleanup()
  })

  it('renders nothing when closed', () => {
    render(QuickAddTask, { props: { open: false, onclose: vi.fn() } })
    expect(screen.queryByPlaceholderText('Task title, link, or note...')).toBeNull()
  })

  it('renders input when open', () => {
    render(QuickAddTask, { props: { open: true, onclose: vi.fn() } })
    expect(screen.getByPlaceholderText('Task title, link, or note...')).toBeDefined()
  })

  it('does not show project row when no projects', () => {
    render(QuickAddTask, { props: { open: true, onclose: vi.fn() } })
    expect(screen.queryByPlaceholderText('Project (optional)...')).toBeNull()
  })

  it('calls onclose when Escape pressed', async () => {
    const onclose = vi.fn()
    render(QuickAddTask, { props: { open: true, onclose } })
    const input = screen.getByPlaceholderText('Task title, link, or note...')
    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(onclose).toHaveBeenCalledOnce()
  })

  it('calls taskStore.create on submit', async () => {
    const onclose = vi.fn()
    const oncreated = vi.fn()
    const created = { id: 'task-new' }
    mockCreate.mockResolvedValue(created)

    render(QuickAddTask, { props: { open: true, onclose, oncreated } })
    const input = screen.getByPlaceholderText('Task title, link, or note...')
    await fireEvent.input(input, { target: { value: 'My new task' } })
    await fireEvent.submit(input.closest('form')!)
    await vi.waitFor(() => {
      expect(mockCreate).toHaveBeenCalledWith('My new task', '', 'interactive')
    })
  })

  it('does not submit when input is empty', async () => {
    const onclose = vi.fn()
    render(QuickAddTask, { props: { open: true, onclose } })
    const form = screen.getByPlaceholderText('Task title, link, or note...').closest('form')!
    await fireEvent.submit(form)
    expect(mockCreate).not.toHaveBeenCalled()
  })

  it('accepts optional oncreated prop', () => {
    const { container } = render(QuickAddTask, {
      props: { open: true, onclose: vi.fn(), oncreated: vi.fn() },
    })
    expect(container).toBeDefined()
  })
})
