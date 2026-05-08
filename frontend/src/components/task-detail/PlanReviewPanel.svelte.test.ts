import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockApprove = vi.fn()
const mockReject = vi.fn()

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    approvePlan: (...args: unknown[]) => mockApprove(...args),
    rejectPlan: (...args: unknown[]) => mockReject(...args),
  },
}))

const PlanReviewPanel = (await import('./PlanReviewPanel.svelte')).default

const baseTask = {
  id: 't1',
  title: 'Plan it',
  status: 'plan-review',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('PlanReviewPanel', () => {
  beforeEach(() => {
    mockApprove.mockReset()
    mockReject.mockReset()
  })
  afterEach(cleanup)

  it('does not render when status is not plan-review', () => {
    const { container } = render(PlanReviewPanel, {
      props: { task: { ...baseTask, status: 'todo' } as never },
    })
    expect(container.querySelector('button')).toBeNull()
  })

  it('renders Approve / Reject buttons on plan-review status', () => {
    render(PlanReviewPanel, { props: { task: baseTask as never } })
    expect(screen.getByText('Approve Plan')).toBeDefined()
    expect(screen.getByText('Reject Plan')).toBeDefined()
  })

  it('calls approvePlan on Approve click', async () => {
    mockApprove.mockResolvedValue(baseTask)
    render(PlanReviewPanel, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('Approve Plan'))
    expect(mockApprove).toHaveBeenCalledWith('t1')
  })

  it('calls rejectPlan with feedback on Reject click', async () => {
    mockReject.mockResolvedValue(baseTask)
    render(PlanReviewPanel, { props: { task: baseTask as never } })
    const textarea = screen.getByPlaceholderText('Rejection feedback (optional)...') as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'Needs more detail' } })
    await fireEvent.click(screen.getByText('Reject Plan'))
    expect(mockReject).toHaveBeenCalledWith('t1', 'Needs more detail')
  })

  it('renders Review Plan link when onreviewplan provided', () => {
    const onreviewplan = vi.fn()
    render(PlanReviewPanel, { props: { task: baseTask as never, onreviewplan } })
    expect(screen.getByText('Review Plan →')).toBeDefined()
  })
})
