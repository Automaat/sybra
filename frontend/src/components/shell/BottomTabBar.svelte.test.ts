import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockReset = vi.fn()
vi.mock('../../lib/navigation.svelte.js', () => ({
  navStore: { reset: (...a: unknown[]) => mockReset(...a), get activeTab() { return 'board' } },
}))
vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: { byStatus: () => [] },
}))
vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: { get list() { return [] } },
}))

const BottomTabBar = (await import('./BottomTabBar.svelte')).default

describe('BottomTabBar', () => {
  afterEach(() => {
    cleanup()
    mockReset.mockClear()
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
})
