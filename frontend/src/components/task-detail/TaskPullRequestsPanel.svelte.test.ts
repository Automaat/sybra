import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockByTask = vi.fn()
const mockOpenURL = vi.fn()

vi.mock('../../stores/reviews.svelte.js', () => ({
  reviewStore: {
    byTask: (...args: unknown[]) => mockByTask(...args),
  },
}))

vi.mock('$lib/api', () => ({
  BrowserOpenURL: (...args: unknown[]) => mockOpenURL(...args),
}))

const TaskPullRequestsPanel = (await import('./TaskPullRequestsPanel.svelte')).default

const baseTask = {
  id: 't1',
  title: 'X',
  status: 'in-review',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  prNumber: 42,
  projectId: 'foo/bar',
}

describe('TaskPullRequestsPanel', () => {
  beforeEach(() => {
    mockByTask.mockReset()
    mockOpenURL.mockReset()
    mockByTask.mockReturnValue([])
  })
  afterEach(cleanup)

  it('renders nothing when no PR data and no prNumber', () => {
    const { container } = render(TaskPullRequestsPanel, {
      props: { task: { ...baseTask, prNumber: 0, projectId: '' } as never },
    })
    expect(container.querySelector('button')).toBeNull()
  })

  it('renders fallback PR link when only prNumber + projectId set', () => {
    render(TaskPullRequestsPanel, { props: { task: baseTask as never } })
    expect(screen.getByText('foo/bar#42')).toBeDefined()
  })

  it('opens fallback PR url on click', async () => {
    render(TaskPullRequestsPanel, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('foo/bar#42'))
    expect(mockOpenURL).toHaveBeenCalledWith('https://github.com/foo/bar/pull/42')
  })

  it('renders linked PRs when reviewStore returns them', () => {
    mockByTask.mockReturnValue([
      {
        number: 42,
        title: 'Wire up auth',
        url: 'https://example/42',
        repository: 'foo/bar',
        author: 'alice',
        ciStatus: 'SUCCESS',
        reviewDecision: 'APPROVED',
        unresolvedCount: 0,
        isDraft: false,
      },
    ])
    render(TaskPullRequestsPanel, { props: { task: baseTask as never } })
    expect(screen.getByText('Wire up auth')).toBeDefined()
    expect(screen.getByText(/foo\/bar#42 by alice/)).toBeDefined()
    expect(screen.getByText('Approved')).toBeDefined()
  })

  it('renders Changes / Review needed / Draft / unresolved badges', () => {
    mockByTask.mockReturnValue([
      {
        number: 1,
        title: 'A',
        url: 'u1',
        repository: 'foo/bar',
        author: 'a',
        ciStatus: 'FAILURE',
        reviewDecision: 'CHANGES_REQUESTED',
        unresolvedCount: 3,
        isDraft: true,
      },
      {
        number: 2,
        title: 'B',
        url: 'u2',
        repository: 'foo/bar',
        author: 'b',
        ciStatus: 'PENDING',
        reviewDecision: 'REVIEW_REQUIRED',
        unresolvedCount: 0,
        isDraft: false,
      },
    ])
    render(TaskPullRequestsPanel, { props: { task: baseTask as never } })
    expect(screen.getByText('Changes')).toBeDefined()
    expect(screen.getByText('Review needed')).toBeDefined()
    expect(screen.getByText('Draft')).toBeDefined()
    expect(screen.getByText('3 unresolved')).toBeDefined()
  })

  it('opens linked PR url on click', async () => {
    mockByTask.mockReturnValue([
      {
        number: 1,
        title: 'A',
        url: 'https://example/1',
        repository: 'foo/bar',
        author: 'a',
        unresolvedCount: 0,
      },
    ])
    render(TaskPullRequestsPanel, { props: { task: baseTask as never } })
    await fireEvent.click(screen.getByText('A'))
    expect(mockOpenURL).toHaveBeenCalledWith('https://example/1')
  })
})
