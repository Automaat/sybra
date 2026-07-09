// Browser (Web Notifications API) bridge for web mode: shows an OS
// notification for a live backend notification event while the tab is open.
// No offline/service-worker push — nothing fires once the tab is closed.
//
// Permission is only ever requested from requestEnable(), which callers must
// invoke from an explicit user gesture (a Settings toggle click). Nothing in
// this module calls Notification.requestPermission() on its own.

const STORAGE_KEY = 'sybra.browserNotificationsEnabled'

function safeLocalStorage(): Storage | null {
  try {
    if (typeof localStorage === 'undefined') return null
    const probeKey = '__sybra_ls_probe__'
    localStorage.setItem(probeKey, '1')
    localStorage.removeItem(probeKey)
    return localStorage
  } catch {
    return null
  }
}

export type BrowserNotificationPermission = 'unsupported' | 'granted' | 'denied' | 'default'

class BrowserNotificationStore {
  enabled = $state(false)

  constructor() {
    this.enabled = safeLocalStorage()?.getItem(STORAGE_KEY) === '1'
  }

  get supported(): boolean {
    return typeof window !== 'undefined' && 'Notification' in window
  }

  get permission(): BrowserNotificationPermission {
    if (!this.supported) return 'unsupported'
    return window.Notification.permission
  }

  /** Must be called from a user gesture (e.g. a Settings checkbox onclick). */
  async requestEnable(): Promise<BrowserNotificationPermission> {
    if (!this.supported) return 'unsupported'
    let perm = window.Notification.permission
    if (perm === 'default') {
      perm = await window.Notification.requestPermission()
    }
    this.enabled = perm === 'granted'
    this.persist()
    return perm
  }

  disable(): void {
    this.enabled = false
    this.persist()
  }

  private persist(): void {
    safeLocalStorage()?.setItem(STORAGE_KEY, this.enabled ? '1' : '0')
  }
}

export const browserNotificationStore = new BrowserNotificationStore()

// Module-level so it survives across repeated listen()/unlisten() cycles —
// dedupe must hold for the life of the tab, not just one subscription.
const shownIds = new Set<string>()

export type ShowableNotification = {
  id: string
  title: string
  message: string
  taskId?: string
}

/**
 * Displays a live backend notification as a browser notification, subject to
 * the user's preference/permission. Safe to call unconditionally from the
 * live event stream — it no-ops when disabled, unsupported, not granted, or
 * already shown for this id (buffered/replayed history included).
 */
export function showBrowserNotification(
  n: ShowableNotification,
  onNavigateTask?: (taskId: string) => void,
): void {
  if (!browserNotificationStore.enabled) return
  if (browserNotificationStore.permission !== 'granted') return
  if (shownIds.has(n.id)) return
  shownIds.add(n.id)

  const browserNotif = new window.Notification(n.title, { body: n.message, tag: n.id })
  if (n.taskId) {
    const taskId = n.taskId
    browserNotif.onclick = () => {
      window.focus()
      onNavigateTask?.(taskId)
    }
  }
}

/** Test-only: clears the dedupe set between cases. */
export function resetShownBrowserNotifications(): void {
  shownIds.clear()
}
