import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent, screen } from '@testing-library/svelte'

let resolveAdd: ((value: unknown) => void) | null = null

vi.mock('$lib/api', () => ({
  ListReviewComments: vi.fn().mockResolvedValue([]),
  AddReviewComment: vi.fn(
    () =>
      new Promise((resolve) => {
        resolveAdd = resolve
      }),
  ),
  ResolveReviewComment: vi.fn().mockResolvedValue(undefined),
  DeleteReviewComment: vi.fn().mockResolvedValue(undefined),
}))

const { commentStore } = await import('../stores/comments.svelte.js')
const { default: PlanFileView } = await import('./PlanFileView.svelte')
const { task } = await import('../../wailsjs/go/models.js')

describe('PlanFileView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    ;(commentStore as unknown as { byTask: Map<string, unknown> }).byTask.clear()
    resolveAdd = null
  })

  it('renders optimistic comment immediately, before backend resolves', async () => {
    render(PlanFileView, {
      props: { taskId: 'task-1', planBody: 'first line\nsecond line' },
    })

    const addButtons = screen.getAllByTitle('Add comment')
    await fireEvent.click(addButtons[0])

    const textarea = screen.getByPlaceholderText('Add a comment...')
    await fireEvent.input(textarea, { target: { value: 'needs work' } })

    const submit = screen.getByRole('button', { name: 'Add Comment' })
    await fireEvent.click(submit)

    // Backend promise is still pending (resolveAdd not called).
    // The optimistic comment must already be visible — this guards the
    // Svelte 5 Map reactivity pitfall that caused the perceived delay.
    expect(screen.getByText('needs work')).toBeDefined()

    resolveAdd?.(
      task.ReviewComment.createFrom({
        id: 'persisted-1',
        line: 1,
        body: 'needs work',
        resolved: false,
        createdAt: '2026-04-10T00:00:00Z',
      }),
    )
  })
})
