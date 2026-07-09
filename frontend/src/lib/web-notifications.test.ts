import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

const {
  browserNotificationStore,
  showBrowserNotification,
  resetShownBrowserNotifications,
} = await import('./web-notifications.js')

// jsdom does not implement the Notification API, so each test installs its
// own fake and cleans it up — this also lets us cover the "unsupported"
// browser case by simply not installing one.
function installFakeNotification(permission: NotificationPermission) {
  const instances: Array<{ title: string; options: unknown; onclick: (() => void) | null }> = []
  class FakeNotification {
    static permission = permission
    static requestPermission = vi.fn(async () => FakeNotification.permission)
    onclick: (() => void) | null = null
    constructor(public title: string, public options: unknown) {
      instances.push(this)
    }
  }
  ;(window as unknown as { Notification: unknown }).Notification = FakeNotification
  return { FakeNotification, instances }
}

function uninstallNotification() {
  delete (window as unknown as { Notification?: unknown }).Notification
}

describe('browserNotificationStore', () => {
  beforeEach(() => {
    localStorage.clear()
    browserNotificationStore.disable()
    resetShownBrowserNotifications()
  })

  afterEach(() => {
    uninstallNotification()
  })

  it('is unsupported when the browser has no Notification API', () => {
    uninstallNotification()
    expect(browserNotificationStore.supported).toBe(false)
    expect(browserNotificationStore.permission).toBe('unsupported')
  })

  it('reflects granted/denied/default permission when supported', () => {
    installFakeNotification('granted')
    expect(browserNotificationStore.permission).toBe('granted')

    installFakeNotification('denied')
    expect(browserNotificationStore.permission).toBe('denied')

    installFakeNotification('default')
    expect(browserNotificationStore.permission).toBe('default')
  })

  it('requestEnable() is a no-op returning unsupported when the API is missing', async () => {
    uninstallNotification()
    const result = await browserNotificationStore.requestEnable()
    expect(result).toBe('unsupported')
    expect(browserNotificationStore.enabled).toBe(false)
  })

  it('requestEnable() prompts only when permission is default, then enables on grant', async () => {
    const { FakeNotification } = installFakeNotification('default')
    FakeNotification.requestPermission = vi.fn(async () => 'granted')

    const result = await browserNotificationStore.requestEnable()

    expect(FakeNotification.requestPermission).toHaveBeenCalledTimes(1)
    expect(result).toBe('granted')
    expect(browserNotificationStore.enabled).toBe(true)
  })

  it('requestEnable() does not re-prompt when already granted', async () => {
    const { FakeNotification } = installFakeNotification('granted')

    await browserNotificationStore.requestEnable()

    expect(FakeNotification.requestPermission).not.toHaveBeenCalled()
    expect(browserNotificationStore.enabled).toBe(true)
  })

  it('requestEnable() stays disabled when the user denies the prompt', async () => {
    const { FakeNotification } = installFakeNotification('default')
    FakeNotification.requestPermission = vi.fn(async () => 'denied')

    const result = await browserNotificationStore.requestEnable()

    expect(result).toBe('denied')
    expect(browserNotificationStore.enabled).toBe(false)
  })

  it('requestEnable() stays disabled when permission is already denied (revoked)', async () => {
    installFakeNotification('denied')

    const result = await browserNotificationStore.requestEnable()

    expect(result).toBe('denied')
    expect(browserNotificationStore.enabled).toBe(false)
  })

  it('disable() clears the preference', async () => {
    installFakeNotification('granted')
    await browserNotificationStore.requestEnable()
    expect(browserNotificationStore.enabled).toBe(true)

    browserNotificationStore.disable()

    expect(browserNotificationStore.enabled).toBe(false)
  })

  it('persists the enabled preference across store instances', async () => {
    installFakeNotification('granted')
    await browserNotificationStore.requestEnable()

    vi.resetModules()
    const fresh = await import('./web-notifications.js')

    expect(fresh.browserNotificationStore.enabled).toBe(true)
  })

  it('does not crash when localStorage is unavailable', async () => {
    const original = Object.getOwnPropertyDescriptor(window, 'localStorage')
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      get() { throw new Error('localStorage disabled') },
    })
    try {
      vi.resetModules()
      const fresh = await import('./web-notifications.js')
      expect(fresh.browserNotificationStore.enabled).toBe(false)

      installFakeNotification('granted')
      await expect(fresh.browserNotificationStore.requestEnable()).resolves.toBe('granted')
    } finally {
      if (original) Object.defineProperty(window, 'localStorage', original)
    }
  })
})

describe('showBrowserNotification', () => {
  beforeEach(() => {
    localStorage.clear()
    browserNotificationStore.disable()
    resetShownBrowserNotifications()
  })

  afterEach(() => {
    uninstallNotification()
  })

  it('does nothing when the preference is disabled', async () => {
    const { instances } = installFakeNotification('granted')

    showBrowserNotification({ id: 'n1', title: 'T', message: 'M' })

    expect(instances).toHaveLength(0)
  })

  it('does nothing when unsupported', () => {
    uninstallNotification()
    showBrowserNotification({ id: 'n1', title: 'T', message: 'M' })
    // no throw is the assertion here
  })

  it('does nothing when permission is not granted, even if enabled was set previously', async () => {
    installFakeNotification('granted')
    await browserNotificationStore.requestEnable()
    const { instances } = installFakeNotification('denied')

    showBrowserNotification({ id: 'n1', title: 'T', message: 'M' })

    expect(instances).toHaveLength(0)
  })

  it('shows exactly one notification when enabled and granted', async () => {
    const { instances } = installFakeNotification('granted')
    await browserNotificationStore.requestEnable()

    showBrowserNotification({ id: 'n1', title: 'Hello', message: 'World' })

    expect(instances).toHaveLength(1)
    expect(instances[0].title).toBe('Hello')
  })

  it('dedupes by notification id — a repeat id shows nothing further', async () => {
    const { instances } = installFakeNotification('granted')
    await browserNotificationStore.requestEnable()

    showBrowserNotification({ id: 'dup', title: 'A', message: 'M' })
    showBrowserNotification({ id: 'dup', title: 'B', message: 'M' })

    expect(instances).toHaveLength(1)
    expect(instances[0].title).toBe('A')
  })

  it('invokes onNavigateTask when a taskId-bearing notification is clicked', async () => {
    const { instances } = installFakeNotification('granted')
    await browserNotificationStore.requestEnable()
    const onNavigateTask = vi.fn()

    showBrowserNotification({ id: 'n1', title: 'T', message: 'M', taskId: 'task-42' }, onNavigateTask)
    instances[0].onclick?.()

    expect(onNavigateTask).toHaveBeenCalledWith('task-42')
  })

  it('does not attach a click handler when there is no taskId', async () => {
    const { instances } = installFakeNotification('granted')
    await browserNotificationStore.requestEnable()

    showBrowserNotification({ id: 'n1', title: 'T', message: 'M' })

    expect(instances[0].onclick).toBeNull()
  })
})
