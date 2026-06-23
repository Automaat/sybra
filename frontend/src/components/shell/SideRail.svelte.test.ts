import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'

vi.mock('../../lib/navigation.svelte.js', () => ({
  navStore: {
    page: { kind: 'task-list' },
    reset: vi.fn(),
  },
}))

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    byStatus: vi.fn(() => []),
  },
}))

vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: {
    list: [],
  },
}))

const SideRail = (await import('./SideRail.svelte')).default

describe('SideRail', () => {
  afterEach(() => cleanup())

  it('renders nav element', () => {
    render(SideRail)
    expect(screen.getByRole('navigation')).toBeDefined()
  })

  it('renders all navigation labels', () => {
    render(SideRail)
    expect(screen.getByText('Board')).toBeDefined()
    expect(screen.getByText('Dashboard')).toBeDefined()
    expect(screen.getByText('Projects')).toBeDefined()
    expect(screen.getByText('Chats')).toBeDefined()
    expect(screen.getByText('Agents')).toBeDefined()
    expect(screen.getByText('GitHub')).toBeDefined()
    expect(screen.getByText('Reviews')).toBeDefined()
    expect(screen.getByText('Logbook')).toBeDefined()
    expect(screen.getByText('Workflows')).toBeDefined()
    expect(screen.getByText('Stats')).toBeDefined()
    expect(screen.getByText('Settings')).toBeDefined()
  })

  it('renders the S logo', () => {
    render(SideRail)
    expect(screen.getByText('S')).toBeDefined()
  })

  it('renders the nav group headers', () => {
    render(SideRail)
    for (const group of ['Work', 'Sessions', 'Build', 'Data']) {
      expect(screen.getByText(group)).toBeDefined()
    }
  })

  it('calls navStore.reset when Board clicked', async () => {
    const { navStore } = await import('../../lib/navigation.svelte.js')
    render(SideRail)
    await fireEvent.click(screen.getByText('Board'))
    expect(navStore.reset).toHaveBeenCalledWith({ kind: 'task-list' })
  })

  it('calls navStore.reset when Settings clicked', async () => {
    const { navStore } = await import('../../lib/navigation.svelte.js')
    render(SideRail)
    await fireEvent.click(screen.getByText('Settings'))
    expect(navStore.reset).toHaveBeenCalledWith({ kind: 'settings' })
  })

  it('calls navStore.reset when Dashboard clicked', async () => {
    const { navStore } = await import('../../lib/navigation.svelte.js')
    render(SideRail)
    await fireEvent.click(screen.getByText('Dashboard'))
    expect(navStore.reset).toHaveBeenCalledWith({ kind: 'dashboard' })
  })

  it('does not show agent badge when no running agents', () => {
    const { container } = render(SideRail)
    // Badge appears as small circle with count — no running agents means no badge
    const badges = container.querySelectorAll('.rounded-full.bg-success-500')
    expect(badges).toHaveLength(0)
  })
})
