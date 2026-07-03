import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

vi.mock('../../lib/markdown.js', () => ({
  renderMarkdown: (s: unknown) => (s ? `<p>${s}</p>` : ''),
}))

const TaskReviewPanel = (await import('./TaskReviewPanel.svelte')).default

const baseTask = {
  id: 't1',
  title: 'X',
  status: 'done',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('TaskReviewPanel', () => {
  afterEach(cleanup)

  it('renders nothing when there is no review or review runs', () => {
    const { container } = render(TaskReviewPanel, { props: { task: baseTask as never } })
    expect(container.textContent?.trim()).toBe('')
  })

  it('renders the code review artifact with a single heading', () => {
    render(TaskReviewPanel, { props: { task: { ...baseTask, codeReview: 'review body' } as never } })
    expect(screen.getByText('review body')).toBeDefined()
    expect(screen.getByText('auto-generated')).toBeDefined()
  })

  it('summarises review and test runs with role badges', () => {
    const task = {
      ...baseTask,
      codeReview: 'r',
      agentRuns: [
        { agentId: 'a-impl', role: 'implementation', state: 'done', mode: 'headless', costUsd: 0.4 },
        { agentId: 'a-review', role: 'review', state: 'done', mode: 'headless', costUsd: 0.1 },
        { agentId: 'a-test', role: 'test-runner', state: 'done', mode: 'headless', costUsd: 0.07 },
      ],
    }
    render(TaskReviewPanel, { props: { task: task as never } })
    expect(screen.getByText('Review')).toBeDefined()
    expect(screen.getByText('Test')).toBeDefined()
    // The implementation run is not a review/test run — excluded from this panel.
    expect(screen.queryByText('a-impl')).toBeNull()
  })
})
