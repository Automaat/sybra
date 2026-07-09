import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, cleanup } from '@testing-library/svelte'
import { tick } from 'svelte'

// jsdom has no Web Animations API, which transition:fly relies on for its
// outro. Stub it to a no-op transition so removal from the DOM is
// synchronous with the {#each} update instead of gated on an animation the
// test environment can't run.
vi.mock('svelte/transition', () => ({ fly: () => ({ duration: 0 }) }))

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

const toastContainerModule = await import('./ToastContainer.svelte')
const ToastContainer = toastContainerModule.default
const { pruneHiddenToastIds } = toastContainerModule

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

  it('hides the toast locally when close button clicked, without deleting from the store', async () => {
    mockNotifications.push(makeNotification({ id: 'notif-1' }))
    render(ToastContainer, { props: {} })
    const dismissBtn = screen.getByLabelText('Dismiss')
    await fireEvent.click(dismissBtn)
    expect(screen.queryByText('Test Toast')).toBeNull()
    expect(mockDismiss).not.toHaveBeenCalled()
    expect(mockNotifications).toHaveLength(1)
  })

  it('hides the toast locally when toast body clicked, without deleting from the store', async () => {
    mockNotifications.push(makeNotification({ id: 'notif-1' }))
    render(ToastContainer, { props: {} })
    await fireEvent.click(screen.getByText('Test Toast'))
    expect(screen.queryByText('Test Toast')).toBeNull()
    expect(mockDismiss).not.toHaveBeenCalled()
    expect(mockNotifications).toHaveLength(1)
  })

  it('calls onviewtask when toast with taskId clicked', async () => {
    mockNotifications.push(makeNotification({ id: 'notif-1', taskId: 'task-42' }))
    const onviewtask = vi.fn()
    render(ToastContainer, { props: { onviewtask } })
    await fireEvent.click(screen.getByText('Test Toast'))
    expect(onviewtask).toHaveBeenCalledWith('task-42')
  })

  it('does not call onviewtask when no taskId', async () => {
    mockNotifications.push(makeNotification())
    const onviewtask = vi.fn()
    render(ToastContainer, { props: { onviewtask } })
    await fireEvent.click(screen.getByRole('alert'))
    expect(onviewtask).not.toHaveBeenCalled()
  })

  it('does not resurrect an older, never-shown notification when a visible toast is dismissed', async () => {
    // List is newest-first: [n4, n3, n2, n1]. Only the top 3 (n4, n3, n2)
    // ever render as toasts; n1 sits past the 3-slot window. Dismissing n2
    // must not slide n1 in with a fresh timer.
    for (const id of ['n4', 'n3', 'n2', 'n1']) {
      mockNotifications.push(makeNotification({ id, title: `Toast ${id}` }))
    }
    render(ToastContainer, { props: {} })
    expect(screen.getAllByRole('alert').length).toBe(3)
    expect(screen.queryByText('Toast n1')).toBeNull()

    const n2 = screen.getByText('Toast n2').closest('[role="alert"]')!
    await fireEvent.click(n2.querySelector('[aria-label="Dismiss"]')!)

    expect(screen.queryByText('Toast n2')).toBeNull()
    expect(screen.queryByText('Toast n1')).toBeNull()
    expect(screen.getAllByRole('alert').length).toBe(2)
  })

  it('hides the toast after auto-dismiss without removing it from the store', async () => {
    vi.useFakeTimers()
    try {
      mockNotifications.push(makeNotification({ id: 'notif-1' }))
      render(ToastContainer, { props: {} })
      expect(screen.getByText('Test Toast')).toBeDefined()

      vi.advanceTimersByTime(5000)
      await tick()

      expect(screen.queryByText('Test Toast')).toBeNull()
      expect(mockDismiss).not.toHaveBeenCalled()
      expect(mockNotifications).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
  })

  it('prunes hidden IDs that no longer exist in the notification list', () => {
    const hidden = new Set(['keep', 'drop'])
    const pruned = pruneHiddenToastIds(hidden, [makeNotification({ id: 'keep' })])

    expect([...pruned]).toEqual(['keep'])
  })

  it('reuses the same hidden-ID set when there is nothing to prune', () => {
    const hidden = new Set(['keep'])
    const pruned = pruneHiddenToastIds(hidden, [makeNotification({ id: 'keep' })])

    expect(pruned).toBe(hidden)
  })
})
