import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import TaskCard from './TaskCard.svelte'

const mockTask = {
  id: 'task-1',
  title: 'Test Task',
  status: 'todo',
  agentMode: 'headless',
  allowedTools: [],
  tags: ['backend'],
  projectId: '',
  agentRuns: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  body: '',
  filePath: '',
  convertValues: () => {},
}

describe('TaskCard', () => {
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
