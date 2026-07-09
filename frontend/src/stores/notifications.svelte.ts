import { EventsOn, ListNotifications } from '$lib/api'
import { Level, Notification } from '../../bindings/github.com/Automaat/sybra/internal/notification/models.js'
import { Notification as NotificationEvent } from '../lib/events.js'
import { showBrowserNotification } from '../lib/web-notifications.js'

class NotificationStore {
  notifications = $state<Notification[]>([])

  /** Seeds in-app history only — must never trigger a browser notification. */
  async load(): Promise<void> {
    this.notifications = (await ListNotifications()) ?? []
  }

  /**
   * Subscribes to live backend notification events. onNavigateTask, if given,
   * is called when the user clicks a browser notification for a task.
   */
  listen(onNavigateTask?: (taskId: string) => void): () => void {
    return EventsOn(NotificationEvent, (n: Notification) => {
      this.notifications = [n, ...this.notifications].slice(0, 50)
      showBrowserNotification(
        { id: n.id, title: n.title, message: n.message, taskId: n.taskId },
        onNavigateTask,
      )
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
