import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

vi.mock('../../lib/markdown.js', () => ({
  renderMarkdown: (s: unknown) => (s ? `<p>${s}</p>` : ''),
}))

const TaskPlanPanel = (await import('./TaskPlanPanel.svelte')).default

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

describe('TaskPlanPanel', () => {
  afterEach(cleanup)

  it('renders nothing when the task has no planning artifacts', () => {
    const { container } = render(TaskPlanPanel, { props: { task: baseTask as never } })
    expect(container.textContent?.trim()).toBe('')
  })

  it('renders the plan body and the read-only label', () => {
    render(TaskPlanPanel, { props: { task: { ...baseTask, plan: 'the plan' } as never } })
    expect(screen.getByText('the plan')).toBeDefined()
    expect(screen.getByText('read-only')).toBeDefined()
  })

  it('renders collapsible critique, research, and decision brief when present', () => {
    render(TaskPlanPanel, {
      props: {
        task: {
          ...baseTask,
          plan: 'p',
          planCritique: 'crit',
          planResearch: 'res',
          planDecisions: 'dec',
        } as never,
      },
    })
    expect(screen.getByText('Plan critique')).toBeDefined()
    expect(screen.getByText('Plan research')).toBeDefined()
    expect(screen.getByText('Decision brief')).toBeDefined()
  })
})
