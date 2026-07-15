import { describe, it, expect, vi, afterEach } from 'vitest'
import { tick } from 'svelte'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'

vi.mock('../../lib/navigation.svelte.js', () => ({
  navStore: {
    page: { kind: 'task-list' },
    reset: vi.fn(),
  },
}))

const mockTaskList: any[] = []

vi.mock('../../stores/tasks.svelte.js', () => ({
  taskStore: {
    tasksNeedingPlanApproval: vi.fn(() => []),
    get list() { return mockTaskList },
  },
}))

vi.mock('../../stores/agents.svelte.js', () => ({
  agentStore: {
    list: [],
  },
}))

const mockNotifications: any[] = []

vi.mock('../../stores/notifications.svelte.js', () => ({
  notificationStore: {
    get notifications() { return mockNotifications },
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
    mockTaskList.length = 0
    mockNotifications.length = 0
  })

  it('renders nav element', () => {
    render(SideRail)
    expect(screen.getByRole('navigation')).toBeDefined()
  })

  it('renders all navigation labels', () => {
    render(SideRail)
    expect(screen.getByText('Board')).toBeDefined()
    expect(screen.getByText('Projects')).toBeDefined()
    expect(screen.getByText('Chats')).toBeDefined()
    expect(screen.getByText('Agents')).toBeDefined()
    expect(screen.getByText('GitHub')).toBeDefined()
    expect(screen.getByText('Reviews')).toBeDefined()
    expect(screen.getByText('Logbook')).toBeDefined()
    expect(screen.getByText('Workflows')).toBeDefined()
    expect(screen.getByText('Stats')).toBeDefined()
    expect(screen.getByText('Inbox')).toBeDefined()
    expect(screen.getByText('Settings')).toBeDefined()
  })

  it('calls navStore.reset when Inbox clicked', async () => {
    const { navStore } = await import('../../lib/navigation.svelte.js')
    render(SideRail)
    await fireEvent.click(screen.getByText('Inbox'))
    expect(navStore.reset).toHaveBeenCalledWith({ kind: 'notifications' })
  })

  it('does not show a needs-you badge on Board when no task needs attention', () => {
    render(SideRail)
    expect(screen.queryByLabelText(/task.*need you/)).toBeNull()
  })

  it('shows a needs-you badge on Board when an active task needs attention', () => {
    mockTaskList.push({ id: 't1', status: 'human-required', tags: [] })
    render(SideRail)
    expect(screen.getByLabelText('1 task need you')).toBeDefined()
  })

  it('excludes done tasks from the needs-you badge count', () => {
    mockTaskList.push({ id: 't1', status: 'done', tags: [] })
    render(SideRail)
    expect(screen.queryByLabelText(/task.*need you/)).toBeNull()
  })

  it('renders the S logo', () => {
    render(SideRail)
    expect(screen.getByText('S')).toBeDefined()
  })

  it('has no group headers — the rail is flat', () => {
    render(SideRail)
    for (const group of ['Work', 'Sessions', 'Build', 'Data']) {
      expect(screen.queryByText(group)).toBeNull()
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
    expect(screen.queryByText('Projects')).toBeNull()
    expect(screen.queryByText('Stats')).toBeNull()
    expect(screen.queryByText('Work')).toBeNull()
  })

  it('expands to the full flat nav when More is clicked', async () => {
    focusModeStore.set(true)
    render(SideRail)
    await fireEvent.click(screen.getByText('More'))
    expect(screen.getByText('Projects')).toBeDefined()
    expect(screen.getByText('Stats')).toBeDefined()
    expect(screen.getByText('Less')).toBeDefined()
  })

  it('collapses to minimal reactively when focus mode is enabled after mount', async () => {
    render(SideRail)
    expect(screen.getByText('Projects')).toBeDefined() // full nav while off
    focusModeStore.set(true)
    await tick()
    expect(screen.queryByText('Projects')).toBeNull() // collapsed without remount
    expect(screen.getByText('More')).toBeDefined()
  })

  it('resets the More expansion when focus mode is turned off then on', async () => {
    focusModeStore.set(true)
    render(SideRail)
    await fireEvent.click(screen.getByText('More')) // expand
    expect(screen.getByText('Projects')).toBeDefined()
    focusModeStore.set(false)
    await tick()
    focusModeStore.set(true)
    await tick()
    // Re-enabling starts minimal again, not stuck expanded.
    expect(screen.queryByText('Projects')).toBeNull()
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

  it('does not show agent badge when no running agents', () => {
    const { container } = render(SideRail)
    // Badge appears as small circle with count — no running agents means no badge
    const badges = container.querySelectorAll('.rounded-full.bg-success-500')
    expect(badges).toHaveLength(0)
  })

  it('shows the full Inbox count in accessible text while keeping the badge readable', () => {
    mockNotifications.push(...Array.from({ length: 50 }, (_, i) => ({ id: `n-${i}` })))
    render(SideRail)

    const badge = screen.getByLabelText('50 notifications')
    expect(badge.textContent).toBe('50')
    expect(badge.getAttribute('title')).toBe('50 notifications')
  })
})
