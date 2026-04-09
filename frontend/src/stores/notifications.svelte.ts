import { EventsOn } from '../../wailsjs/runtime/runtime.js'
import { ListNotifications } from '../../wailsjs/go/main/App.js'
import { notification } from '../../wailsjs/go/models.js'
import { Notification as NotificationEvent } from '../lib/events.js'

class NotificationStore {
  notifications = $state<notification.Notification[]>([])

  async load(): Promise<void> {
    this.notifications = (await ListNotifications()) ?? []
  }

  listen(): () => void {
    return EventsOn(NotificationEvent, (n: notification.Notification) => {
      this.notifications = [n, ...this.notifications].slice(0, 50)
    })
  }

  /** Push a transient client-side notification (e.g. for failed actions). */
  pushLocal(level: 'info' | 'success' | 'warning' | 'error', title: string, message: string): void {
    const n = new notification.Notification({
      id: `local-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      level,
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
