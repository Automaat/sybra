// In-app browser preference and link routing.
//
// Opening a GitHub link in Sybra's own webview window keeps the login session in
// one place — the user stays in a single app. The system browser is still one
// modifier-click (or one opt-out) away, so nothing is taken away.
import { OpenInAppBrowser, BrowserOpenURL } from './api.js'

const STORAGE_KEY = 'inAppBrowser'

function loadStored(): boolean {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return v === null ? true : v === 'true' // default: open links in-app
  } catch {
    // localStorage unavailable (SSR / restricted context)
    return true
  }
}

function createStore() {
  let enabled = $state<boolean>(loadStored())

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
