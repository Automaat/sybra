import { EventsOn, ListNotifications } from '$lib/api'
import { Level, Notification } from '../../bindings/github.com/Automaat/sybra/internal/notification/models.js'
import { Notification as NotificationEvent } from '../lib/events.js'

class NotificationStore {
  notifications = $state<Notification[]>([])

  async load(): Promise<void> {
    this.notifications = ((await ListNotifications()) ?? []).slice(0, 50)
  }

  listen(): () => void {
    return EventsOn(NotificationEvent, (n: Notification) => {
      this.notifications = [n, ...this.notifications].slice(0, 50)
    })
  }

  /** Push a transient client-side notification (e.g. for failed actions). */
  pushLocal(level: 'info' | 'success' | 'warning' | 'error', title: string, message: string): void {
    const n = new Notification({
      id: `local-${crypto.randomUUID()}`,
      level: level as Level,
      title,
      message,
      createdAt: new Date().toISOString(),
    })
    this.notifications = [n, ...this.notifications].slice(0, 50)
  }

  dismiss(id: string): void {
    this.notifications = this.notifications.filter((n) => n.id !== id)
  }

  clear(): void {
    this.notifications = []
  }
}

export const notificationStore = new NotificationStore()
