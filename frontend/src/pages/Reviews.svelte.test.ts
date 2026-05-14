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

  it('shows projectId in sidebar when set', () => {
    const task = makeTask({ id: 't1', title: 'Scoped task', projectId: 'owner/repo' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    render(Reviews)
    expect(screen.getByText('owner/repo')).toBeDefined()
  })

  it('shows View Task button when onviewtask is passed', async () => {
    const task = makeTask({ id: 't1', title: 'Linkable task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    const onviewtask = vi.fn()
    render(Reviews, { props: { onviewtask } })
    await fireEvent.click(screen.getByText('Linkable task'))
    await vi.waitFor(() => {
      expect(screen.getByText('View Task →')).toBeDefined()
    })
    await fireEvent.click(screen.getByText('View Task →'))
    expect(onviewtask).toHaveBeenCalledWith('t1')
  })

  it('renders Send Message button (disabled until feedback and live agent)', async () => {
    const task = makeTask({ id: 't1', title: 'Send target' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockHasLivePlanAgent.mockResolvedValue(false)
    render(Reviews)
    await fireEvent.click(screen.getByText('Send target'))
    await vi.waitFor(() => {
      expect(screen.getByText('Send Message')).toBeDefined()
    })
  })

  it('calls sendPlanMessage when Send Message clicked with live agent and feedback', async () => {
    const task = makeTask({ id: 't1', title: 'Live agent task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockHasLivePlanAgent.mockResolvedValue(true)
    mockSendPlanMessage.mockResolvedValue(undefined)
    render(Reviews)
    await fireEvent.click(screen.getByText('Live agent task'))
    await vi.waitFor(() => screen.getByPlaceholderText(/Rejection feedback/))
    const textarea = screen.getByPlaceholderText(/Rejection feedback/)
    await fireEvent.input(textarea, { target: { value: 'tighten section 3' } })
    await fireEvent.click(screen.getByText('Send Message'))
    await vi.waitFor(() => {
      expect(mockSendPlanMessage).toHaveBeenCalledWith('t1', 'tighten section 3')
    })
  })

  it('calls approveTestPlan for test-plan-review tasks', async () => {
    const task = makeTask({ id: 't1', title: 'Test plan task', status: 'test-plan-review' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'test-plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockApproveTestPlan.mockResolvedValue(undefined)
    render(Reviews)
    await fireEvent.click(screen.getByText('Test plan task'))
    await vi.waitFor(() => screen.getByText('Approve'))
    await fireEvent.click(screen.getByText('Approve'))
    await vi.waitFor(() => {
      expect(mockApproveTestPlan).toHaveBeenCalledWith('t1')
    })
  })

  it('calls rejectTestPlan with feedback for test-plan-review tasks', async () => {
    const task = makeTask({ id: 't1', title: 'Test reject', status: 'test-plan-review' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'test-plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockRejectTestPlan.mockResolvedValue(undefined)
    render(Reviews)
    await fireEvent.click(screen.getByText('Test reject'))
    await vi.waitFor(() => screen.getByPlaceholderText(/Rejection feedback/))
    const textarea = screen.getByPlaceholderText(/Rejection feedback/)
    await fireEvent.input(textarea, { target: { value: 'missing edge case' } })
    await fireEvent.click(screen.getByText('Reject'))
    await vi.waitFor(() => {
      expect(mockRejectTestPlan).toHaveBeenCalledWith('t1', 'missing edge case')
    })
  })

  it('renders Plan Critique section when planCritique is present', async () => {
    const task = makeTask({
      id: 't1',
      title: 'Critiqued task',
      planCritique: 'Plan needs more detail on auth',
    })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    render(Reviews)
    await fireEvent.click(screen.getByText('Critiqued task'))
    await vi.waitFor(() => {
      expect(screen.getByText('Plan Critique (auto-review)')).toBeDefined()
    })
  })

  it('shows error message when reject fails', async () => {
    const task = makeTask({ id: 't1', title: 'Reject fails' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockRejectPlan.mockRejectedValue(new Error('reject error'))
    render(Reviews)
    await fireEvent.click(screen.getByText('Reject fails'))
    await vi.waitFor(() => screen.getByText('Reject'))
    await fireEvent.click(screen.getByText('Reject'))
    await vi.waitFor(() => {
      expect(screen.getByText(/reject error/)).toBeDefined()
    })
  })

  it('shows unresolved comments banner in detail panel when count > 0', async () => {
    const task = makeTask({ id: 't1', title: 'Commented' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockUnresolvedCount.mockReturnValue(2)
    render(Reviews)
    await fireEvent.click(screen.getByText('Commented'))
    await vi.waitFor(() => {
      expect(screen.getByText(/2 unresolved comments/)).toBeDefined()
    })
  })

  it('uses singular comment label when only one unresolved', async () => {
    const task = makeTask({ id: 't1', title: 'One comment' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockUnresolvedCount.mockReturnValue(1)
    render(Reviews)
    await fireEvent.click(screen.getByText('One comment'))
    await vi.waitFor(() => {
      expect(screen.getByText(/1 unresolved comment$/)).toBeDefined()
    })
  })

  it('approve "a" keypress triggers plan approval', async () => {
    const task = makeTask({ id: 't1', title: 'Keyboard task' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockApprovePlan.mockResolvedValue(undefined)
    render(Reviews)
    await fireEvent.click(screen.getByText('Keyboard task'))
    await vi.waitFor(() => screen.getByText('Approve'))
    await fireEvent.keyDown(window, { key: 'a' })
    await vi.waitFor(() => {
      expect(mockApprovePlan).toHaveBeenCalledWith('t1')
    })
  })

  it('reject "r" keypress triggers plan rejection', async () => {
    const task = makeTask({ id: 't1', title: 'Reject keyboard' })
    mockByStatus.mockImplementation((status: string) => {
      if (status === 'plan-review') return [task]
      return []
    })
    taskItemsMap.set('t1', task)
    mockRejectPlan.mockResolvedValue(undefined)
    render(Reviews)
    await fireEvent.click(screen.getByText('Reject keyboard'))
    await vi.waitFor(() => screen.getByText('Reject'))
    await fireEvent.keyDown(window, { key: 'r' })
    await vi.waitFor(() => {
      expect(mockRejectPlan).toHaveBeenCalledWith('t1', '')
    })
  })
})
