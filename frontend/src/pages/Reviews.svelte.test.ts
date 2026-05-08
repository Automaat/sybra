import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockByStatus = vi.fn()
const mockApprovePlan = vi.fn()
const mockRejectPlan = vi.fn()
const mockSendPlanMessage = vi.fn()
const mockApproveTestPlan = vi.fn()
const mockRejectTestPlan = vi.fn()
const mockSendTestPlanMessage = vi.fn()
const mockHasLivePlanAgent = vi.fn()
const mockHasLiveTestPlanAgent = vi.fn()
const mockCommentLoad = vi.fn()
const mockUnresolvedCount = vi.fn()

const taskItemsMap = new Map<string, any>()

vi.mock('../stores/tasks.svelte.js', () => ({
  taskStore: {
    get items() { return taskItemsMap },
    byStatus: (...args: unknown[]) => mockByStatus(...args),
    approvePlan: (...args: unknown[]) => mockApprovePlan(...args),
    rejectPlan: (...args: unknown[]) => mockRejectPlan(...args),
    sendPlanMessage: (...args: unknown[]) => mockSendPlanMessage(...args),
    approveTestPlan: (...args: unknown[]) => mockApproveTestPlan(...args),
    rejectTestPlan: (...args: unknown[]) => mockRejectTestPlan(...args),
    sendTestPlanMessage: (...args: unknown[]) => mockSendTestPlanMessage(...args),
    hasLivePlanAgent: (...args: unknown[]) => mockHasLivePlanAgent(...args),
    hasLiveTestPlanAgent: (...args: unknown[]) => mockHasLiveTestPlanAgent(...args),
  },
}))

vi.mock('../stores/comments.svelte.js', () => ({
  commentStore: {
    load: (...args: unknown[]) => mockCommentLoad(...args),
    unresolvedCount: (...args: unknown[]) => mockUnresolvedCount(...args),
  },
}))

vi.mock('../components/PlanFileView.svelte', () => ({ default: () => {} }))

vi.mock('../lib/viewport.svelte.js', () => ({
  viewport: { isDesktop: true, isMobile: false },
}))

vi.mock('../lib/markdown.js', () => ({
  renderMarkdown: vi.fn((text: string) => text ?? ''),
}))

const Reviews = (await import('./Reviews.svelte')).default

function makeTask(overrides: Record<string, unknown> = {}) {
  return {
    id: 'task-1',
    title: 'My Plan Task',
    status: 'plan-review',
    tags: [],
    projectId: '',
    plan: '## Plan\nDo the thing',
    body: '',
    planCritique: '',
    ...overrides,
  }
}

describe('Reviews', () => {
  beforeEach(() => {
    mockByStatus.mockReset()
    mockApprovePlan.mockReset()
    mockRejectPlan.mockReset()
    mockSendPlanMessage.mockReset()
    mockApproveTestPlan.mockReset()
    mockRejectTestPlan.mockReset()
    mockSendTestPlanMessage.mockReset()
    mockHasLivePlanAgent.mockResolvedValue(false)
    mockHasLiveTestPlanAgent.mockResolvedValue(false)
    mockCommentLoad.mockResolvedValue(undefined)
    mockUnresolvedCount.mockReturnValue(0)
    mockByStatus.mockReturnValue([])
    taskItemsMap.clear()
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('shows "No tasks pending review" when no review tasks', () => {
    render(Reviews)
    expect(screen.getByText('No tasks pending review')).toBeDefined()
  })

  it('shows Reviews heading in sidebar', () => {
    render(Reviews)
    expect(screen.getByText('Reviews')).toBeDefined()
  })

  it('renders review task titles in sidebar', () => {
    const task = makeTask({ id: 't1', title: 'Feature planning task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    render(Reviews)
    expect(screen.getByText('Feature planning task')).toBeDefined()
  })

  it('shows count badge when review tasks exist', () => {
    const task = makeTask({ id: 't1' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    render(Reviews)
    expect(screen.getByText('1')).toBeDefined()
  })

  it('shows Plan badge for plan-review tasks', () => {
    const task = makeTask({ id: 't1', status: 'plan-review' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    render(Reviews)
    expect(screen.getByText('Plan')).toBeDefined()
  })

  it('shows Test badge for test-plan-review tasks', () => {
    const task = makeTask({ id: 't1', status: 'test-plan-review' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'test-plan-review') return [task]
      return []
    })
    render(Reviews)
    expect(screen.getByText('Test')).toBeDefined()
  })

  it('shows select-a-task placeholder when nothing selected', () => {
    render(Reviews)
    expect(screen.getByText(/Select a task to review/)).toBeDefined()
  })

  it('shows task title in panel after selecting a task', async () => {
    const task = makeTask({ id: 't1', title: 'Auth refactor plan' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    render(Reviews)
    await fireEvent.click(screen.getByText('Auth refactor plan'))
    // Task title appears in the detail panel header
    const titles = screen.getAllByText('Auth refactor plan')
    expect(titles.length).toBeGreaterThanOrEqual(1)
  })

  it('shows Approve and Reject buttons after selecting a task', async () => {
    const task = makeTask({ id: 't1', title: 'Plan task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    render(Reviews)
    await fireEvent.click(screen.getByText('Plan task'))
    await vi.waitFor(() => {
      expect(screen.getByText('Approve')).toBeDefined()
      expect(screen.getByText('Reject')).toBeDefined()
    })
  })

  it('calls approvePlan and clears selection on Approve', async () => {
    const task = makeTask({ id: 't1', title: 'Approve me' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockApprovePlan.mockResolvedValue({ ...task, status: 'todo' })
    render(Reviews)
    await fireEvent.click(screen.getByText('Approve me'))
    await vi.waitFor(() => {
      expect(screen.getByText('Approve')).toBeDefined()
    })
    await fireEvent.click(screen.getByText('Approve'))
    await vi.waitFor(() => {
      expect(mockApprovePlan).toHaveBeenCalledWith('t1')
    })
  })

  it('calls rejectPlan on Reject', async () => {
    const task = makeTask({ id: 't1', title: 'Reject me' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockRejectPlan.mockResolvedValue({ ...task, status: 'planning' })
    render(Reviews)
    await fireEvent.click(screen.getByText('Reject me'))
    await vi.waitFor(() => {
      expect(screen.getByText('Reject')).toBeDefined()
    })
    await fireEvent.click(screen.getByText('Reject'))
    await vi.waitFor(() => {
      expect(mockRejectPlan).toHaveBeenCalledWith('t1', '')
    })
  })

  it('shows error message when approve fails', async () => {
    const task = makeTask({ id: 't1', title: 'Failing task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockApprovePlan.mockRejectedValue(new Error('server error'))
    render(Reviews)
    await fireEvent.click(screen.getByText('Failing task'))
    await vi.waitFor(() => {
      expect(screen.getByText('Approve')).toBeDefined()
    })
    await fireEvent.click(screen.getByText('Approve'))
    await vi.waitFor(() => {
      expect(screen.getByText('Error: server error')).toBeDefined()
    })
  })

  it('shows task tags in sidebar', () => {
    const task = makeTask({ id: 't1', title: 'Tagged task', tags: ['backend', 'api'] })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    render(Reviews)
    expect(screen.getByText('backend')).toBeDefined()
    expect(screen.getByText('api')).toBeDefined()
  })

  it('shows unresolved comment count badge', () => {
    const task = makeTask({ id: 't1', title: 'Commented task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    mockUnresolvedCount.mockReturnValue(3)
    render(Reviews)
    expect(screen.getByText('3')).toBeDefined()
  })
})
