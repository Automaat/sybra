import { EventsOn } from '../../wailsjs/runtime/runtime.js'
import { ListNotifications } from '../../wailsjs/go/main/App.js'
import type { notification } from '../../wailsjs/go/models.js'
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

  dismiss(id: string): void {
    this.notifications = this.notifications.filter((n) => n.id !== id)
  }

  clear(): void {
    this.notifications = []
  }
}

export const notificationStore = new NotificationStore()
