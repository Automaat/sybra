import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import TaskCard from './TaskCard.svelte'
import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import { PullRequest } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

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

  it('does not show a pill for the default headless mode', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.queryByText('headless')).toBeNull()
  })

  it('marks the minority interactive mode by exception', () => {
    render(TaskCard, { props: { task: { ...mockTask, agentMode: 'interactive' }, onclick: () => {} } })
    expect(screen.getByText('interactive')).toBeDefined()
  })

  it('renders tags when present', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.getByText('backend')).toBeDefined()
  })

  it('shows umbrella progress for tracker cards', () => {
    render(TaskCard, {
      props: {
        task: { ...mockTask, taskType: 'umbrella' } as Task,
        umbrellaProgress: { done: 3, total: 10 },
        onclick: () => {},
      },
    })
    expect(screen.getByText('3/10')).toBeDefined()
    expect(screen.getByTitle('3/10 subissues complete')).toBeDefined()
  })

  it('hides empty umbrella progress', () => {
    render(TaskCard, {
      props: {
        task: { ...mockTask, taskType: 'umbrella' } as Task,
        umbrellaProgress: { done: 0, total: 0 },
        onclick: () => {},
      },
    })
    expect(screen.queryByText('0/0')).toBeNull()
  })

  it('does not render tags section when empty', () => {
    const taskNoTags = { ...mockTask, tags: [] }
    render(TaskCard, { props: { task: taskNoTags, onclick: () => {} } })
    expect(screen.queryByText('backend')).toBeNull()
  })

  it('calls onclick handler when clicked', async () => {
    const handler = vi.fn()
    render(TaskCard, { props: { task: mockTask, onclick: handler } })
    await fireEvent.click(screen.getByText('Test Task'))
    expect(handler).toHaveBeenCalledOnce()
  })

  it('always renders the task actions menu trigger', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.getByLabelText('Task actions')).toBeDefined()
  })

  it('copies the task ID to the clipboard', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    await fireEvent.click(screen.getByLabelText('Task actions'))
    await fireEvent.click(screen.getByText('Copy task ID'))
    expect(writeText).toHaveBeenCalledWith('task-1')
  })

  it('only offers issue/PR copy actions when the task has them', async () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    await fireEvent.click(screen.getByLabelText('Task actions'))
    expect(screen.queryByText('Copy issue link')).toBeNull()
    expect(screen.queryByText('Copy PR link')).toBeNull()
  })

  it('offers copy issue/PR links when present', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })
    render(TaskCard, {
      props: {
        task: {
          ...mockTask,
          issue: 'https://github.com/owner/repo/issues/7',
          prNumber: 42,
          projectId: 'owner/repo',
        } as Task,
        onclick: () => {},
      },
    })
    await fireEvent.click(screen.getByLabelText('Task actions'))
    await fireEvent.click(screen.getByText('Copy issue link'))
    expect(writeText).toHaveBeenCalledWith('https://github.com/owner/repo/issues/7')
    await fireEvent.click(screen.getByLabelText('Task actions'))
    await fireEvent.click(screen.getByText('Copy PR link'))
    expect(writeText).toHaveBeenCalledWith('https://github.com/owner/repo/pull/42')
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

  it('shows a quiet sub-state badge for a folded non-attention status', () => {
    // `new` rolls up to the Todo column; surface the precise sub-state so the
    // two board axes (stage column vs sub-state) stay separate.
    render(TaskCard, { props: { task: { ...mockTask, status: 'new' as Task['status'] }, onclick: () => {} } })
    expect(screen.getByText('New')).toBeDefined()
  })

  it('does not show a sub-state badge for a plain core status', () => {
    render(TaskCard, { props: { task: mockTask, onclick: () => {} } })
    expect(screen.queryByText('Todo')).toBeNull()
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

  it('shows the drafted phase badge for a review task with a pending draft', () => {
    reviewStore.reviewRequested = [makePR()]
    render(TaskCard, {
      props: {
        task: { ...mockTask, tags: ['review'], projectId: 'owner/repo', prNumber: 42, reviewPhase: 'drafted' } as Task,
        onclick: () => {},
      },
    })

    expect(screen.getByText('Post review')).toBeDefined()
    expect(screen.getByText(/#42/)).toBeDefined()
  })

  it('shows the approved phase badge for an approved review task', () => {
    reviewStore.reviewedByMe = [makePR({ viewerHasApproved: true })]
    render(TaskCard, {
      props: {
        task: { ...mockTask, tags: ['review'], projectId: 'owner/repo', prNumber: 42, reviewPhase: 'approved' } as Task,
        onclick: () => {},
      },
    })

    expect(screen.getByText('Approved')).toBeDefined()
    expect(screen.getByText(/#42/)).toBeDefined()
  })

  it('keeps an accessible "issue exists" cue when a PR and an issue coexist', () => {
    render(TaskCard, {
      props: {
        task: { ...mockTask, prNumber: 42, issue: 'https://github.com/owner/repo/issues/7' } as Task,
        onclick: () => {},
      },
    })
    // The PR is the primary reference; the issue must not be silently dropped.
    expect(screen.getByText(/#42/)).toBeDefined()
    const cue = screen.getByLabelText('Also linked to an issue')
    expect(cue).toBeDefined()
    expect(cue.getAttribute('title')).toBe('https://github.com/owner/repo/issues/7')
  })

  it('shows a standalone issue reference when there is no PR', () => {
    render(TaskCard, {
      props: {
        task: { ...mockTask, issue: 'https://github.com/owner/repo/issues/7' } as Task,
        onclick: () => {},
      },
    })
    expect(screen.getByText('Issue')).toBeDefined()
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
      const { container } = render(TaskCard, { props: { task: taskNullDate, onclick: () => {} } })
      const timeSpan = container.querySelector('.ml-auto')
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
