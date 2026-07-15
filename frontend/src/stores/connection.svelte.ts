import { GetVersion } from '$lib/api'

// How often to probe the backend when the browser reports online.
const POLL_MS = 15_000
// How often to probe when browser reports offline (less aggressive).
const OFFLINE_POLL_MS = 5_000

class ConnectionStore {
  /** True when the Sybra backend is reachable. */
  backendOnline = $state(true)
  /** True when the browser's network interface is up. */
  networkOnline = $state(typeof navigator !== 'undefined' ? navigator.onLine : true)

  private timer: ReturnType<typeof setInterval> | null = null

  get online(): boolean {
    return this.networkOnline && this.backendOnline
  }

  private async probe() {
    // Desktop (Wails IPC): backend is always local, only network matters.
    // Web build: GetVersion is a lightweight health check.
    if (import.meta.env.VITE_MODE !== 'web') {
      this.backendOnline = true
      return
    }
    try {
      await GetVersion()
      this.backendOnline = true
    } catch {
      this.backendOnline = false
    }
  }

  start(): () => void {
    if (typeof window === 'undefined') return () => {}

    const onOnline = () => {
      this.networkOnline = true
      this.probe()
    }
    const onOffline = () => {
      this.networkOnline = false
      this.backendOnline = false
    }

    window.addEventListener('online', onOnline)
    window.addEventListener('offline', onOffline)

    const schedule = () => {
      if (this.timer) clearInterval(this.timer)
      const interval = this.online ? POLL_MS : OFFLINE_POLL_MS
      this.timer = setInterval(() => {
        this.probe().then(() => schedule())
      }, interval)
    }

    this.probe().then(() => schedule())

    return () => {
      window.removeEventListener('online', onOnline)
      window.removeEventListener('offline', onOffline)
      if (this.timer) {
        clearInterval(this.timer)
        this.timer = null
      }
    }
  }
}

export const connectionStore = new ConnectionStore()
