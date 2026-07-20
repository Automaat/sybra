import { describe, it, expect, beforeEach, vi } from 'vitest'

const { mockOpenInApp, mockSystem, mockGetSettings } = vi.hoisted(() => ({
  mockOpenInApp: vi.fn(),
  mockSystem: vi.fn(),
  mockGetSettings: vi.fn<() => Promise<{ browser: { inApp: boolean | null } }>>(
    async () => ({ browser: { inApp: null } }),
  ),
}))

vi.mock('./api.js', () => ({
  GetSettings: mockGetSettings,
  OpenInAppBrowser: mockOpenInApp,
  BrowserOpenURL: mockSystem,
}))

import { openLink, inAppBrowserStore } from './browser.svelte.js'

const URL = 'https://github.com/o/r/issues/1'

describe('openLink routing', () => {
  beforeEach(() => {
    mockOpenInApp.mockClear()
    mockSystem.mockClear()
    mockGetSettings.mockClear()
    mockGetSettings.mockResolvedValue({ browser: { inApp: null } })
    localStorage.clear()
    inAppBrowserStore.set(false)
  })

  it('uses the system browser on a plain click when in-app is omitted', () => {
    openLink(URL, new MouseEvent('click'))
    expect(mockSystem).toHaveBeenCalledWith(URL)
    expect(mockOpenInApp).not.toHaveBeenCalled()
  })

  it('uses the system browser when the preference is explicitly false', () => {
    inAppBrowserStore.set(false)
    openLink(URL)
    expect(mockSystem).toHaveBeenCalledWith(URL)
    expect(mockOpenInApp).not.toHaveBeenCalled()
  })

  it('opens in-app on a plain click when enabled', () => {
    inAppBrowserStore.set(true)
    openLink(URL, new MouseEvent('click'))
    expect(mockOpenInApp).toHaveBeenCalledWith(URL)
    expect(mockSystem).not.toHaveBeenCalled()
  })

  it('escapes to the system browser on modifier click even when enabled', () => {
    inAppBrowserStore.set(true)
    openLink(URL, new MouseEvent('click', { metaKey: true }))
    openLink(URL, new MouseEvent('click', { ctrlKey: true }))
    expect(mockSystem).toHaveBeenCalledTimes(2)
    expect(mockOpenInApp).not.toHaveBeenCalled()
  })
})

describe('inAppBrowserStore persistence', () => {
  beforeEach(() => {
    mockOpenInApp.mockClear()
    mockSystem.mockClear()
    mockGetSettings.mockClear()
    mockGetSettings.mockResolvedValue({ browser: { inApp: null } })
    localStorage.clear()
  })

  it('defaults to disabled when storage is empty', async () => {
    vi.resetModules()
    localStorage.removeItem('inAppBrowser')
    const { inAppBrowserStore: fresh } = await import('./browser.svelte.js')
    expect(fresh.enabled).toBe(false)
  })

  it('reads a stored explicit false', async () => {
    vi.resetModules()
    localStorage.setItem('inAppBrowser', 'false')
    const { inAppBrowserStore: fresh } = await import('./browser.svelte.js')
    expect(fresh.enabled).toBe(false)
  })

  it('reads a stored explicit true', async () => {
    vi.resetModules()
    localStorage.setItem('inAppBrowser', 'true')
    const { inAppBrowserStore: fresh } = await import('./browser.svelte.js')
    expect(fresh.enabled).toBe(true)
  })

  it('hydrates explicit backend opt-in when localStorage is empty', async () => {
    vi.resetModules()
    mockGetSettings.mockResolvedValue({ browser: { inApp: true } })
    const { inAppBrowserStore: fresh } = await import('./browser.svelte.js')
    await vi.waitFor(() => expect(fresh.enabled).toBe(true))
    expect(localStorage.getItem('inAppBrowser')).toBe('true')
  })

  it('set() persists the choice', () => {
    inAppBrowserStore.set(false)
    expect(localStorage.getItem('inAppBrowser')).toBe('false')
    inAppBrowserStore.set(true)
    expect(localStorage.getItem('inAppBrowser')).toBe('true')
  })
})
