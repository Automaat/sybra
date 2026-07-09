import { describe, it, expect, vi, beforeEach } from 'vitest'
import { Notification } from '../../bindings/github.com/Automaat/sybra/internal/notification/models.js'
import { Notification as NotificationEvent } from '../lib/events.js'

const mockListNotifications = vi.fn()
let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('$lib/api', () => ({
  ListNotifications: (...args: unknown[]) => mockListNotifications(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
}))

const { notificationStore } = await import('./notifications.svelte.js')

function makeNotification(overrides: Record<string, unknown> = {}): Notification {
  return Notification.createFrom({
    id: 'n-1',
    level: 'info',
    title: 'Test',
    message: 'Test message',
    createdAt: new Date().toISOString(),
    ...overrides,
  })
}

describe('NotificationStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    eventCallbacks = {}
    notificationStore.notifications = []
    notificationStore.clear()
  })

  describe('load', () => {
    it('fetches notifications from backend', async () => {
      const ns = [makeNotification({ id: 'n1' }), makeNotification({ id: 'n2' })]
      mockListNotifications.mockResolvedValue(ns)

      await notificationStore.load()

      expect(notificationStore.notifications).toHaveLength(2)
    })

    it('handles null result', async () => {
      mockListNotifications.mockResolvedValue(null)

      await notificationStore.load()

      expect(notificationStore.notifications).toHaveLength(0)
    })

    it('caps result at 50 entries, matching the event and local paths', async () => {
      const ns = Array.from({ length: 75 }, (_, i) => makeNotification({ id: `n${i}` }))
      mockListNotifications.mockResolvedValue(ns)

      await notificationStore.load()

      expect(notificationStore.notifications).toHaveLength(50)
      expect(notificationStore.notifications[0].id).toBe('n0')
    })
  })

  describe('listen', () => {
    it('registers event listener', () => {
      const unsub = notificationStore.listen()

      expect(eventCallbacks[NotificationEvent]).toBeDefined()
      unsub()
    })

    it('prepends incoming notification', () => {
      notificationStore.notifications = [makeNotification({ id: 'old' })]
      const unsub = notificationStore.listen()

      eventCallbacks[NotificationEvent](makeNotification({ id: 'new' }))

      expect(notificationStore.notifications[0].id).toBe('new')
      expect(notificationStore.notifications).toHaveLength(2)
      unsub()
    })

    it('caps list at 50 entries', () => {
      notificationStore.notifications = Array.from({ length: 50 }, (_, i) =>
        makeNotification({ id: `old-${i}` }),
      )
      const unsub = notificationStore.listen()

      eventCallbacks[NotificationEvent](makeNotification({ id: 'overflow' }))

      expect(notificationStore.notifications).toHaveLength(50)
      expect(notificationStore.notifications[0].id).toBe('overflow')
      unsub()
    })

    it('unsubscribes listener', () => {
      const unsub = notificationStore.listen()
      unsub()

      expect(eventCallbacks[NotificationEvent]).toBeUndefined()
    })
  })

  describe('pushLocal', () => {
    it('adds notification to front of list', () => {
      notificationStore.pushLocal('error', 'Oops', 'Something went wrong')

      expect(notificationStore.notifications).toHaveLength(1)
      expect(notificationStore.notifications[0].level).toBe('error')
      expect(notificationStore.notifications[0].title).toBe('Oops')
      expect(notificationStore.notifications[0].message).toBe('Something went wrong')
    })

    it('assigns unique id', () => {
      notificationStore.pushLocal('info', 'A', 'msg')
      notificationStore.pushLocal('info', 'B', 'msg')

      const ids = notificationStore.notifications.map((n) => n.id)
      expect(new Set(ids).size).toBe(2)
    })

    it('caps at 50 entries', () => {
      for (let i = 0; i < 51; i++) {
        notificationStore.pushLocal('info', `n${i}`, 'msg')
      }
      expect(notificationStore.notifications).toHaveLength(50)
    })
  })

  describe('dismiss', () => {
    it('removes notification by id', () => {
      notificationStore.notifications = [
        makeNotification({ id: 'keep' }),
        makeNotification({ id: 'remove' }),
      ]

      notificationStore.dismiss('remove')

      expect(notificationStore.notifications).toHaveLength(1)
      expect(notificationStore.notifications[0].id).toBe('keep')
    })

    it('is a no-op for unknown id', () => {
      notificationStore.notifications = [makeNotification({ id: 'n1' })]

      notificationStore.dismiss('nonexistent')

      expect(notificationStore.notifications).toHaveLength(1)
    })
  })

  describe('clear', () => {
    it('empties the list', () => {
      notificationStore.notifications = [makeNotification(), makeNotification()]

      notificationStore.clear()

      expect(notificationStore.notifications).toHaveLength(0)
    })
  })
})
