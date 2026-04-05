import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'

const mockDismiss = vi.fn()
const mockNotifications: any[] = []

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    get notifications() {
      return mockNotifications
    },
    dismiss: (...args: unknown[]) => mockDismiss(...args),
  },
}))

const ToastContainer = (await import('./ToastContainer.svelte')).default

function makeNotification(overrides: Record<string, unknown> = {}) {
  return {
    id: 'notif-1',
    title: 'Test Toast',
    message: 'Something happened',
    level: 'info',
    ...overrides,
  }
}

describe('ToastContainer', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockNotifications.length = 0
  })

  afterEach(() => {
    cleanup()
  })

  it('renders container', () => {
    const { container } = render(ToastContainer, { props: {} })
    expect(container).toBeDefined()
  })

  it('shows no toasts when notifications empty', () => {
    render(ToastContainer, { props: {} })
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('renders toast title and message', () => {
    mockNotifications.push(makeNotification())
    render(ToastContainer, { props: {} })
    expect(screen.getByText('Test Toast')).toBeDefined()
    expect(screen.getByText('Something happened')).toBeDefined()
  })

  it('renders at most 3 toasts', () => {
    for (let i = 0; i < 5; i++) {
      mockNotifications.push(makeNotification({ id: `n-${i}`, title: `Toast ${i}` }))
    }
    render(ToastContainer, { props: {} })
    const alerts = screen.getAllByRole('alert')
    expect(alerts.length).toBe(3)
  })

  it('calls dismiss when close button clicked', async () => {
    mockNotifications.push(makeNotification({ id: 'notif-1' }))
    render(ToastContainer, { props: {} })
    const dismissBtn = screen.getByLabelText('Dismiss')
    await fireEvent.click(dismissBtn)
    expect(mockDismiss).toHaveBeenCalledWith('notif-1')
  })

  it('calls dismiss when toast body clicked', async () => {
    mockNotifications.push(makeNotification({ id: 'notif-1' }))
    render(ToastContainer, { props: {} })
    const alert = screen.getByRole('alert')
    await fireEvent.click(alert)
    expect(mockDismiss).toHaveBeenCalledWith('notif-1')
  })

  it('calls onviewtask when toast with taskId clicked', async () => {
    mockNotifications.push(makeNotification({ id: 'notif-1', taskId: 'task-42' }))
    const onviewtask = vi.fn()
    render(ToastContainer, { props: { onviewtask } })
    const alert = screen.getByRole('alert')
    await fireEvent.click(alert)
    expect(onviewtask).toHaveBeenCalledWith('task-42')
  })

  it('does not call onviewtask when no taskId', async () => {
    mockNotifications.push(makeNotification())
    const onviewtask = vi.fn()
    render(ToastContainer, { props: { onviewtask } })
    await fireEvent.click(screen.getByRole('alert'))
    expect(onviewtask).not.toHaveBeenCalled()
  })
})
