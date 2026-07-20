// In-app browser preference and link routing.
//
// Opening a GitHub link in Sybra's own webview window keeps the login session in
// one place — the user stays in a single app. The system browser is still one
// modifier-click (or one opt-out) away, so nothing is taken away.
import { GetSettings, OpenInAppBrowser, BrowserOpenURL } from './api.js'

const STORAGE_KEY = 'inAppBrowser'

function loadStored(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) === 'true'
  } catch {
    // localStorage unavailable (SSR / restricted context)
    return false
  }
}

function createStore() {
  let enabled = $state<boolean>(loadStored())

  // Keep explicit browser.inApp: true installs enabled even when localStorage
  // has never been written on this machine.
  void Promise.resolve(GetSettings()).then((settings) => {
    if (settings?.browser?.inApp === true) {
      enabled = true
      try {
        localStorage.setItem(STORAGE_KEY, 'true')
      } catch {
        // ignore
      }
    }
  }).catch(() => {})

  return {
    get enabled(): boolean {
      return enabled
    },
    set(v: boolean): void {
      enabled = v
      try {
        localStorage.setItem(STORAGE_KEY, String(v))
      } catch {
        // ignore
      }
    },
  }
}

export const inAppBrowserStore = createStore()

// openLink routes an external URL to the in-app browser window, or to the system
// browser when the preference is off or the user holds ⌘/Ctrl (the standard
// "open elsewhere" gesture).
export function openLink(url: string, e?: MouseEvent | KeyboardEvent): void {
  const wantsSystem = !!e && (e.metaKey || e.ctrlKey)
  if (inAppBrowserStore.enabled && !wantsSystem) {
    OpenInAppBrowser(url)
  } else {
    BrowserOpenURL(url)
  }
}
