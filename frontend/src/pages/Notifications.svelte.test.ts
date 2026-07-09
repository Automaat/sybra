import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockDismiss = vi.fn()
const mockClear = vi.fn()
const mockNotifications: any[] = []

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    get notifications() { return mockNotifications },
    dismiss: (...args: unknown[]) => mockDismiss(...args),
    clear: (...args: unknown[]) => mockClear(...args),
  },
}))

const Notifications = (await import('./Notifications.svelte')).default

function makeNotification(overrides: Record<string, unknown> = {}) {
  return {
    id: 'n-1',
    level: 'info',
    title: 'Agent finished',
    message: 'Task ready for review',
    createdAt: new Date().toISOString(),
    ...overrides,
  }
}

describe('Notifications', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockNotifications.length = 0
  })

  afterEach(() => {
    cleanup()
  })

  it('shows an empty state when there are no notifications', () => {
    render(Notifications, { props: {} })
    expect(screen.getByText('No notifications yet')).toBeDefined()
  })

  it('renders notification title, message, and level', () => {
    mockNotifications.push(makeNotification())
    render(Notifications, { props: {} })
    expect(screen.getByText('Agent finished')).toBeDefined()
    expect(screen.getByText('Task ready for review')).toBeDefined()
    expect(screen.getByText('info')).toBeDefined()
  })

  it('renders a notification whose toast already auto-dismissed — the inbox is the retention layer', () => {
    // Simulates: toast for this id expired client-side, but it was never
    // removed from the store (ToastContainer no longer calls dismiss()).
    mockNotifications.push(makeNotification({ id: 'expired-toast' }))
    render(Notifications, { props: {} })
    expect(screen.getByText('Agent finished')).toBeDefined()
  })

  it('shows a count of notifications', () => {
    mockNotifications.push(makeNotification({ id: 'n1' }), makeNotification({ id: 'n2' }))
    render(Notifications, { props: {} })
    expect(screen.getByText('2 notifications')).toBeDefined()
  })

  it('dismisses a single notification via its row action', async () => {
    mockNotifications.push(makeNotification({ id: 'n-1' }))
    render(Notifications, { props: {} })
    await fireEvent.click(screen.getByText('Dismiss'))
    expect(mockDismiss).toHaveBeenCalledWith('n-1')
  })

  it('clears all notifications via the clear-all action', async () => {
    mockNotifications.push(makeNotification({ id: 'n1' }), makeNotification({ id: 'n2' }))
    render(Notifications, { props: {} })
    await fireEvent.click(screen.getByText('Clear all'))
    expect(mockClear).toHaveBeenCalledOnce()
  })

  it('does not show clear-all when there are no notifications', () => {
    render(Notifications, { props: {} })
    expect(screen.queryByText('Clear all')).toBeNull()
  })

  it('navigates to the linked task when View task is clicked', async () => {
    mockNotifications.push(makeNotification({ id: 'n-1', taskId: 'task-42' }))
    const onviewtask = vi.fn()
    render(Notifications, { props: { onviewtask } })
    await fireEvent.click(screen.getByText('View task →'))
    expect(onviewtask).toHaveBeenCalledWith('task-42')
  })

  it('navigates to the linked agent when View agent is clicked', async () => {
    mockNotifications.push(makeNotification({ id: 'n-1', agentId: 'agent-7' }))
    const onviewagent = vi.fn()
    render(Notifications, { props: { onviewagent } })
    await fireEvent.click(screen.getByText('View agent →'))
    expect(onviewagent).toHaveBeenCalledWith('agent-7')
  })

  it('does not show task/agent navigation actions when no ids are present', () => {
    mockNotifications.push(makeNotification())
    render(Notifications, { props: { onviewtask: vi.fn(), onviewagent: vi.fn() } })
    expect(screen.queryByText('View task →')).toBeNull()
    expect(screen.queryByText('View agent →')).toBeNull()
  })
})
