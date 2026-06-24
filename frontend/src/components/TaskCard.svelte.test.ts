import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import TaskCard from './TaskCard.svelte'
import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import { PullRequest } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: vi.fn(),
  },
}))

const { notificationStore } = await import('../stores/notifications.svelte.js')
const { reviewStore } = await import('../stores/reviews.svelte.js')

const mockTask = {
  id: 'task-1',
  slug: 'test-task',
  title: 'Test Task',
  status: 'todo',
  agentMode: 'headless',
  allowedTools: [],
  tags: ['backend'],
  projectId: '',
  branch: '',
  prNumber: 0,
  issue: '',
  statusReason: '',
  reviewed: false,
  runRole: '',
  agentRuns: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  body: '',
  plan: '',
  planCritique: '',
  codeReview: '',
  filePath: '',
  taskType: '',
  todoistId: '',
  convertValues: () => {},
} as unknown as Task

function makePR(overrides: Record<string, unknown> = {}) {
  return PullRequest.createFrom({
    number: 42,
    title: 'Review PR',
    url: 'https://github.com/owner/repo/pull/42',
    repository: 'owner/repo',
    repoName: 'repo',
    author: 'peer',
    isDraft: false,
    labels: [],
    headRefName: '',
    ciStatus: '',
    reviewDecision: '',
    mergeable: '',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-01T00:00:00Z',
    updatedAt: '2026-04-01T00:00:00Z',
    ...overrides,
  })
}

describe('TaskCard', () => {
  beforeEach(() => {
    reviewStore.createdByMe = []
    reviewStore.reviewRequested = []
    reviewStore.reviewedByMe = []
  })

  afterEach(() => {
    cleanup()
  })

  it('renders task title', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.getByText('Test Task')).toBeDefined()
  })

  it('renders agent mode', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.getByText('headless')).toBeDefined()
  })

  it('renders tags when present', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.getByText('backend')).toBeDefined()
  })

  it('does not render tags section when empty', () => {
    const taskNoTags = { ...mockTask, tags: [] }
    render(TaskCard, { props: { task: taskNoTags, onclick: () => {} } })
    expect(screen.queryByText('backend')).toBeNull()
  })

  it('calls onclick handler when clicked', async () => {
    const handler = vi.fn()
    render(TaskCard, { props: { task: mockTask, onclick: handler } })
    await fireEvent.click(screen.getByRole('button'))
    expect(handler).toHaveBeenCalledOnce()
  })

  it('does not render the move menu without onstatuschange', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.queryByLabelText('Move task')).toBeNull()
  })

  it('opens the move menu and changes status via the picker', async () => {
    const onstatuschange = vi.fn()
    render(TaskCard, { props: { task: mockTask, onclick: () => {}, onstatuschange } })
    await fireEvent.click(screen.getByLabelText('Move task'))
    await fireEvent.click(screen.getByText('In Progress'))
    expect(onstatuschange).toHaveBeenCalledWith('in-progress')
  })

  describe('copyBranch error handling', () => {
    beforeEach(() => {
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: vi.fn() },
        configurable: true,
        writable: true,
      })
      vi.mocked(notificationStore.pushLocal).mockClear()
    })

    afterEach(() => {
      vi.restoreAllMocks()
    })

    it('shows notification when clipboard write fails', async () => {
      vi.mocked(navigator.clipboard.writeText).mockRejectedValueOnce(new Error('Permission denied'))
      const taskWithProject = { ...mockTask, projectId: 'owner/repo' }
      render(TaskCard, { props: { task: taskWithProject, onclick: () => {} } })

      const copyBtn = screen.getByTitle('Copy branch name (⇧⌘.)')
      await fireEvent.click(copyBtn)

      expect(notificationStore.pushLocal).toHaveBeenCalledWith('error', 'Copy failed', 'Could not copy branch name to clipboard')
    })

    it('copies branch name on success', async () => {
      vi.mocked(navigator.clipboard.writeText).mockResolvedValueOnce(undefined)
      const taskWithProject = { ...mockTask, projectId: 'owner/repo' }
      render(TaskCard, { props: { task: taskWithProject, onclick: () => {} } })

      const copyBtn = screen.getByTitle('Copy branch name (⇧⌘.)')
      await fireEvent.click(copyBtn)

      expect(navigator.clipboard.writeText).toHaveBeenCalledWith(expect.stringContaining(mockTask.id))
      expect(notificationStore.pushLocal).not.toHaveBeenCalled()
    })
  })

  it('shows the canonical Plan Review badge for plan-review status', () => {
    render(TaskCard, { props: { task: { ...mockTask, status: 'plan-review' as Task['status'] }, onclick: () => {} } })
    expect(screen.getByText('Plan Review')).toBeDefined()
  })

  it('does not show an attention badge for non-awaiting statuses', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.queryByText('Plan Review')).toBeNull()
  })

  it('shows the canonical Human Required badge for human-required status', () => {
    render(TaskCard, { props: { task: { ...mockTask, status: 'human-required' as Task['status'] }, onclick: () => {} } })
    expect(screen.getByText('Human Required')).toBeDefined()
  })

  it('shows Blocked badge for blocked status', () => {
    render(TaskCard, { props: { task: { ...mockTask, status: 'blocked' as Task['status'] }, onclick: () => {} } })
    expect(screen.getByText('Blocked')).toBeDefined()
  })

  it('adds red left-border accent for awaits-human statuses', () => {
    const { container } = render(TaskCard, {
      props: { task: { ...mockTask, status: 'plan-review' as Task['status'] }, onclick: () => {} },
    })
    expect(container.querySelector('.border-l-error-500')).not.toBeNull()
  })

  it('omits the awaits-human accent for agent-driven statuses', () => {
    const { container } = render(TaskCard, {
      props: { task: { ...mockTask, status: 'in-review' as Task['status'] }, onclick: () => {} },
    })
    expect(container.querySelector('.border-l-error-500')).toBeNull()
  })

  it('shows review-needed badge for review-requested PR tasks', () => {
    reviewStore.reviewRequested = [makePR()]
    render(TaskCard, {
      props: {
        task: { ...mockTask, tags: ['review'], projectId: 'owner/repo', prNumber: 42 } as Task,
        onclick: () => {},
      },
    })

    expect(screen.getByText(/Review needed/)).toBeDefined()
  })

  it('shows approved waiting-merge badge for approved review tasks', () => {
    reviewStore.reviewedByMe = [makePR({ viewerHasApproved: true })]
    render(TaskCard, {
      props: {
        task: { ...mockTask, tags: ['review'], projectId: 'owner/repo', prNumber: 42 } as Task,
        onclick: () => {},
      },
    })

    expect(screen.getByText(/Approved, waiting merge/)).toBeDefined()
  })

  describe('timeAgo', () => {
    beforeEach(() => {
      vi.useFakeTimers()
    })

    afterEach(() => {
      vi.useRealTimers()
      cleanup()
    })

    it('returns empty string for falsy date', () => {
      const taskNullDate = { ...mockTask, updatedAt: '' }
      vi.setSystemTime(new Date('2026-04-01T12:00:00Z'))
      render(TaskCard, { props: { task: taskNullDate, onclick: () => {} } })
      const timeSpan = screen.getByText('headless').parentElement?.querySelector('.ml-auto')
      expect(timeSpan?.textContent).toBe('')
    })

    it('returns "just now" for <60s', () => {
      vi.setSystemTime(new Date('2026-04-01T00:00:30Z'))
      render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
      expect(screen.getByText('just now')).toBeDefined()
    })

    it('returns "Xm ago" for <1h', () => {
      vi.setSystemTime(new Date('2026-04-01T00:05:00Z'))
      render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
      expect(screen.getByText('5m ago')).toBeDefined()
    })

    it('returns "Xh ago" for <24h', () => {
      vi.setSystemTime(new Date('2026-04-01T03:00:00Z'))
      render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
      expect(screen.getByText('3h ago')).toBeDefined()
    })

    it('returns "Xd ago" for >=24h', () => {
      vi.setSystemTime(new Date('2026-04-03T00:00:00Z'))
      render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
      expect(screen.getByText('2d ago')).toBeDefined()
    })
  })
})
