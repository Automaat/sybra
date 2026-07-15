import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockReset = vi.fn()
const mockNotifications: any[] = []
vi.mock('../../lib/navigation.svelte.js', () => ({
  navStore: { reset: (...a: unknown[]) => mockReset(...a) },
}))
vi.mock('../../stores/notifications.svelte.js', () => ({
  notificationStore: {
    get notifications() { return mockNotifications },
  },
}))

const MoreSheet = (await import('./MoreSheet.svelte')).default

describe('MoreSheet', () => {
  afterEach(() => {
    cleanup()
    mockReset.mockClear()
    mockNotifications.length = 0
  })

  it('lists Logbook (reachable on mobile) and not Reviews (it lives in the tab bar)', () => {
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    expect(screen.getByText('Logbook')).toBeDefined()
    // Reviews is a primary bottom-tab; it must not be duplicated here.
    expect(screen.queryByText('Reviews')).toBeNull()
  })

  it('navigates to the logbook when its entry is tapped', async () => {
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    await fireEvent.click(screen.getByText('Logbook'))
    expect(mockReset).toHaveBeenCalledWith({ kind: 'logbook' })
  })

  it('lists an Inbox entry that navigates to notifications', async () => {
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    await fireEvent.click(screen.getByText('Inbox'))
    expect(mockReset).toHaveBeenCalledWith({ kind: 'notifications' })
  })

  it('does not show a badge on Inbox when there are no notifications', () => {
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    expect(screen.queryByText('0')).toBeNull()
  })

  it('shows a badge on Inbox with the current notification count', () => {
    mockNotifications.push({ id: 'n1' }, { id: 'n2' })
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    expect(screen.getByText('2')).toBeDefined()
  })
})
