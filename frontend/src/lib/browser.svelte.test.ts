import { describe, it, expect, beforeEach, vi } from 'vitest'

const mockOpenInApp = vi.fn()
const mockSystem = vi.fn()

vi.mock('./api.js', () => ({
  OpenInAppBrowser: (...args: unknown[]) => mockOpenInApp(...args),
  BrowserOpenURL: (...args: unknown[]) => mockSystem(...args),
}))

import { openLink, inAppBrowserStore } from './browser.svelte.js'

const URL = 'https://github.com/o/r/issues/1'

describe('openLink routing', () => {
  beforeEach(() => {
    mockOpenInApp.mockClear()
    mockSystem.mockClear()
    localStorage.clear()
    inAppBrowserStore.set(true)
  })

  it('opens in-app on a plain click when enabled', () => {
    openLink(URL, new MouseEvent('click'))
    expect(mockOpenInApp).toHaveBeenCalledWith(URL)
    expect(mockSystem).not.toHaveBeenCalled()
  })

  it('opens in-app when no event is supplied', () => {
    openLink(URL)
    expect(mockOpenInApp).toHaveBeenCalledWith(URL)
    expect(mockSystem).not.toHaveBeenCalled()
  })

  it('escapes to the system browser on ⌘-click', () => {
    openLink(URL, new MouseEvent('click', { metaKey: true }))
    expect(mockSystem).toHaveBeenCalledWith(URL)
    expect(mockOpenInApp).not.toHaveBeenCalled()
  })

  it('escapes to the system browser on Ctrl-click', () => {
    openLink(URL, new MouseEvent('click', { ctrlKey: true }))
    expect(mockSystem).toHaveBeenCalledWith(URL)
    expect(mockOpenInApp).not.toHaveBeenCalled()
  })

  it('uses the system browser for every click when the preference is off', () => {
    inAppBrowserStore.set(false)
    openLink(URL, new MouseEvent('click'))
    expect(mockSystem).toHaveBeenCalledWith(URL)
    expect(mockOpenInApp).not.toHaveBeenCalled()
  })
})

describe('inAppBrowserStore persistence', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('defaults to enabled when storage is empty', async () => {
    vi.resetModules()
    localStorage.removeItem('inAppBrowser')
    const { inAppBrowserStore: fresh } = await import('./browser.svelte.js')
    expect(fresh.enabled).toBe(true)
  })

  it('reads a stored opt-out', async () => {
    vi.resetModules()
    localStorage.setItem('inAppBrowser', 'false')
    const { inAppBrowserStore: fresh } = await import('./browser.svelte.js')
    expect(fresh.enabled).toBe(false)
  })

  it('set() persists the choice', () => {
    inAppBrowserStore.set(false)
    expect(localStorage.getItem('inAppBrowser')).toBe('false')
    inAppBrowserStore.set(true)
    expect(localStorage.getItem('inAppBrowser')).toBe('true')
  })
})
