import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockReset = vi.fn()
const mockTaskList: any[] = []
vi.mock('../../lib/navigation.svelte.js', () => ({
  navStore: { reset: (...a: unknown[]) => mockReset(...a), get activeTab() { return 'board' } },
}))
vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: { tasksNeedingPlanApproval: () => [], get list() { return mockTaskList } },
}))
vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: { get list() { return [] } },
}))

const BottomTabBar = (await import('./BottomTabBar.svelte')).default

describe('BottomTabBar', () => {
  afterEach(() => {
    cleanup()
    mockReset.mockClear()
    mockTaskList.length = 0
  })

  it('exposes the primary tabs incl. Reviews (its only mobile home)', () => {
    render(BottomTabBar, { props: { onmore: vi.fn() } })
    for (const label of ['Chats', 'Agents', 'Reviews']) {
      expect(screen.getByLabelText(label)).toBeDefined()
    }
  })

  it('navigates to reviews from the Reviews tab', async () => {
    render(BottomTabBar, { props: { onmore: vi.fn() } })
    await fireEvent.click(screen.getByLabelText('Reviews'))
    expect(mockReset).toHaveBeenCalledWith({ kind: 'reviews' })
  })

  it('opens the More sheet', async () => {
    const onmore = vi.fn()
    render(BottomTabBar, { props: { onmore } })
    await fireEvent.click(screen.getByLabelText('More'))
    expect(onmore).toHaveBeenCalledOnce()
  })

  it('does not show a needs-you badge on Board when no task needs attention', () => {
    render(BottomTabBar, { props: { onmore: vi.fn() } })
    expect(screen.queryByTitle(/task.*need you/)).toBeNull()
  })

  it('shows a needs-you badge on Board when an active task needs attention', () => {
    mockTaskList.push({ id: 't1', status: 'blocked', tags: [] })
    render(BottomTabBar, { props: { onmore: vi.fn() } })
    expect(screen.getByTitle('1 task need you')).toBeDefined()
  })

  it('excludes cancelled tasks from the needs-you badge count', () => {
    mockTaskList.push({ id: 't1', status: 'cancelled', tags: [] })
    render(BottomTabBar, { props: { onmore: vi.fn() } })
    expect(screen.queryByTitle(/task.*need you/)).toBeNull()
  })
})
