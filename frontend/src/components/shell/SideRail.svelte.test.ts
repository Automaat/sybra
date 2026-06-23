import { describe, it, expect, vi, afterEach } from 'vitest'
import { tick } from 'svelte'
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

// Use the real focus-mode store (a $state-backed singleton) so the tests
// exercise genuine cross-component reactivity, not a frozen mock.
const { focusModeStore } = await import('../../lib/focus-mode.svelte.js')

const SideRail = (await import('./SideRail.svelte')).default

describe('SideRail', () => {
  afterEach(() => {
    cleanup()
    focusModeStore.set(false)
  })

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

  it('in focus mode shows only primary items + More, hiding the rest', () => {
    focusModeStore.set(true)
    render(SideRail)
    for (const label of ['Board', 'Reviews', 'Chats', 'Agents']) {
      expect(screen.getByText(label)).toBeDefined()
    }
    expect(screen.getByText('More')).toBeDefined()
    // Non-primary destinations and group headers are tucked away until expanded.
    expect(screen.queryByText('Dashboard')).toBeNull()
    expect(screen.queryByText('Stats')).toBeNull()
    expect(screen.queryByText('Work')).toBeNull()
  })

  it('expands to the full grouped nav when More is clicked', async () => {
    focusModeStore.set(true)
    render(SideRail)
    await fireEvent.click(screen.getByText('More'))
    expect(screen.getByText('Dashboard')).toBeDefined()
    expect(screen.getByText('Stats')).toBeDefined()
    expect(screen.getByText('Less')).toBeDefined()
  })

  it('collapses to minimal reactively when focus mode is enabled after mount', async () => {
    render(SideRail)
    expect(screen.getByText('Dashboard')).toBeDefined() // full nav while off
    focusModeStore.set(true)
    await tick()
    expect(screen.queryByText('Dashboard')).toBeNull() // collapsed without remount
    expect(screen.getByText('More')).toBeDefined()
  })

  it('resets the More expansion when focus mode is turned off then on', async () => {
    focusModeStore.set(true)
    render(SideRail)
    await fireEvent.click(screen.getByText('More')) // expand
    expect(screen.getByText('Dashboard')).toBeDefined()
    focusModeStore.set(false)
    await tick()
    focusModeStore.set(true)
    await tick()
    // Re-enabling starts minimal again, not stuck expanded.
    expect(screen.queryByText('Dashboard')).toBeNull()
    expect(screen.getByText('More')).toBeDefined()
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
