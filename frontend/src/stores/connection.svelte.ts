import { GetVersion, OnConnectionChange, getConnectionState } from '$lib/api'

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
  private reconnectHandlers = new Set<() => void>()

  get online(): boolean {
    return this.networkOnline && this.backendOnline
  }

  /**
   * onReconnect registers work to run once the live stream comes back after a
   * loss. Events emitted while it was down were never delivered, so whatever
   * they would have updated has to be refetched instead.
   */
  onReconnect(handler: () => void): () => void {
    this.reconnectHandlers.add(handler)
    return () => { this.reconnectHandlers.delete(handler) }
  }

  private async probe() {
    // GetVersion is the lightest call the server answers, and every build
    // reaches the server over HTTP now.
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

    // The event stream notices a server that went away long before the next
    // poll would, so it drives the state and the poll stays as the backstop
    // for a server that answers requests but never opened a stream.
    if (getConnectionState() === 'lost') this.backendOnline = false
    const stopStream = OnConnectionChange((state, reconnected) => {
      this.backendOnline = state !== 'lost'
      if (!reconnected) return
      for (const handler of this.reconnectHandlers) handler()
    })

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
      stopStream()
      if (this.timer) {
        clearInterval(this.timer)
        this.timer = null
      }
    }
  }
}

export const connectionStore = new ConnectionStore()
