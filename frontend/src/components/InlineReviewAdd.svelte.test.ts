import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockCreate = vi.fn()
const mockPushLocal = vi.fn()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    create: (...args: unknown[]) => mockCreate(...args),
  },
}))
vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: { pushLocal: (...args: unknown[]) => mockPushLocal(...args) },
}))

const InlineReviewAdd = (await import('./InlineReviewAdd.svelte')).default

describe('InlineReviewAdd', () => {
  beforeEach(() => {
    mockCreate.mockReset()
    mockPushLocal.mockReset()
    mockCreate.mockResolvedValue({ id: 'new1' })
  })
  afterEach(cleanup)

  it('renders the trigger button', () => {
    render(InlineReviewAdd)
    expect(screen.getByText('Add review link')).toBeDefined()
  })

  it('click reveals the input', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    expect(await screen.findByPlaceholderText('Paste a GitHub PR link…')).toBeDefined()
  })

  it('Enter on a valid PR link creates the task', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/pull/123' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockCreate).toHaveBeenCalledWith('https://github.com/owner/repo/pull/123', '', 'headless')
    })
  })

  it('strips a query/fragment glued onto the PR number before creating', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/pull/123#discussion_r1' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockCreate).toHaveBeenCalledWith('https://github.com/owner/repo/pull/123', '', 'headless')
    })
  })

  it('accepts a trailing path segment after the PR number', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/pull/123/files' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockCreate).toHaveBeenCalledWith('https://github.com/owner/repo/pull/123/files', '', 'headless')
    })
  })

  it('rejects a non-PR link without calling create', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/issues/123' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockPushLocal).toHaveBeenCalled()
    })
    expect(mockCreate).not.toHaveBeenCalled()
  })

  it('Escape dismisses without creating', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/pull/123' } })
    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(mockCreate).not.toHaveBeenCalled()
  })

  it('empty link on Enter is a no-op', async () => {
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.keyDown(input, { key: 'Enter' })
    expect(mockCreate).not.toHaveBeenCalled()
  })

  it('create error surfaces notification', async () => {
    mockCreate.mockRejectedValue(new Error('boom'))
    render(InlineReviewAdd)
    await fireEvent.click(screen.getByRole('button'))
    const input = (await screen.findByPlaceholderText('Paste a GitHub PR link…')) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'https://github.com/owner/repo/pull/123' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => {
      expect(mockPushLocal).toHaveBeenCalled()
    })
  })
})
