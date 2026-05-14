import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockCreate = vi.fn()
const mockUpdate = vi.fn()
const mockPushLocal = vi.fn()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    create: (...args: unknown[]) => mockCreate(...args),
    update: (...args: unknown[]) => mockUpdate(...args),
  },
}))
vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: { pushLocal: (...args: unknown[]) => mockPushLocal(...args) },
}))

const InlineTaskAdd = (await import('./InlineTaskAdd.svelte')).default

describe('InlineTaskAdd', () => {
  beforeEach(() => {
    mockCreate.mockReset()
    mockUpdate.mockReset()
    mockPushLocal.mockReset()
    mockCreate.mockResolvedValue({ id: 'new1' })
    mockUpdate.mockResolvedValue({})
  })
  afterEach(cleanup)

  it('renders the trigger button', () => {
    render(InlineTaskAdd, { props: { status: 'todo' } })
    expect(screen.getByText('Add task')).toBeDefined()
  })

  it('click reveals the input', async () => {
    render(InlineTaskAdd, { props: { status: 'todo' } })
    await fireEvent.click(screen.getByRole('button'))
    expect(await screen.findByPlaceholderText('Task title')).toBeDefined()
  })

  it('Enter creates task and applies status for non-"new" columns', async () => {
    render(InlineTaskAdd, { props: { status: 'in-progress' } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Task title')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'New widget' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockCreate).toHaveBeenCalledWith('New widget', '', 'headless')
    })
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith('new1', { status: 'in-progress' })
    })
  })

  it('Enter on "new" column does not call update', async () => {
    render(InlineTaskAdd, { props: { status: 'new' } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Task title')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Fresh idea' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockCreate).toHaveBeenCalled()
    })
    expect(mockUpdate).not.toHaveBeenCalled()
  })

  it('Escape dismisses without creating', async () => {
    render(InlineTaskAdd, { props: { status: 'todo' } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Task title')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Abandon' } })
    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(mockCreate).not.toHaveBeenCalled()
  })

  it('empty title on Enter is a no-op', async () => {
    render(InlineTaskAdd, { props: { status: 'todo' } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Task title')) as HTMLInputElement
    await fireEvent.keyDown(input, { key: 'Enter' })
    expect(mockCreate).not.toHaveBeenCalled()
  })

  it('create error surfaces notification', async () => {
    mockCreate.mockRejectedValue(new Error('boom'))
    render(InlineTaskAdd, { props: { status: 'todo' } })
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Task title')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Will fail' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockPushLocal).toHaveBeenCalled()
    })
  })
})
