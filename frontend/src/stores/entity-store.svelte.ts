// Minimum gap between backend fetches. A burst of load() calls inside this
// window collapses to one leading fetch plus one trailing fetch.
const MIN_LOAD_INTERVAL_MS = 500

export class EntityStore<T extends { id: string }> {
  items = $state<Map<string, T>>(new Map())
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null
  private trailingTimer: ReturnType<typeof setTimeout> | null = null
  private trailingPending = false
  private inFlight: Promise<void> | null = null
  private lastLoadAt = 0

  constructor(
    private readonly loadFn: () => Promise<T[]>,
    private readonly sortFn: (a: T, b: T) => number,
  ) {}

  get list(): T[] {
    return [...this.items.values()].sort(this.sortFn)
  }

  protected set(id: string, item: T): void {
    this.items = new Map(this.items).set(id, item)
  }

  protected delete(id: string): void {
    const next = new Map(this.items)
    next.delete(id)
    this.items = next
  }

  async load(): Promise<void> {
    // A fetch already in flight absorbs this caller, but flags a trailing
    // fetch: the running request may have read the backend before the change
    // that triggered this call landed.
    if (this.inFlight) {
      this.trailingPending = true
      return this.inFlight
    }
    // Throttle to one fetch per MIN_LOAD_INTERVAL_MS. Initial loads (empty
    // items) always fetch so first render is never delayed. A throttled call
    // is never dropped — it schedules a trailing fetch, otherwise a live
    // event (task:created/updated/deleted) arriving just after a load would
    // be invisible until the next poll, up to 30s later.
    const isInitial = this.items.size === 0
    if (!isInitial) {
      const sinceLast = Date.now() - this.lastLoadAt
      if (sinceLast < MIN_LOAD_INTERVAL_MS) {
        this.scheduleTrailingLoad(MIN_LOAD_INTERVAL_MS - sinceLast)
        return
      }
    }
    return this.runLoad(isInitial)
  }

  private scheduleTrailingLoad(delayMs: number): void {
    if (this.trailingTimer !== null) return
    this.trailingTimer = setTimeout(() => {
      this.trailingTimer = null
      void this.load()
    }, delayMs)
  }

  private runLoad(isInitial: boolean): Promise<void> {
    if (isInitial) this.loading = true
    this.error = ''
    this.inFlight = (async () => {
      try {
        const result = await this.loadFn()
        const map = new Map<string, T>()
        for (const item of result ?? []) map.set(item.id, item)
        this.items = map
      } catch (e) {
        this.error = String(e)
      } finally {
        if (isInitial) this.loading = false
        this.lastLoadAt = Date.now()
        this.inFlight = null
        if (this.trailingPending) {
          this.trailingPending = false
          this.scheduleTrailingLoad(MIN_LOAD_INTERVAL_MS)
        }
      }
    })()
    return this.inFlight
  }

  startPolling(interval = 30000): void {
    this.stopPolling()
    this.pollTimer = setInterval(() => this.load(), interval)
  }

  stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer)
      this.pollTimer = null
    }
    if (this.trailingTimer) {
      clearTimeout(this.trailingTimer)
      this.trailingTimer = null
    }
    this.trailingPending = false
  }
}
