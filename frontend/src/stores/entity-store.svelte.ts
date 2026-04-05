export class EntityStore<T extends { id: string }> {
  items = $state<Map<string, T>>(new Map())
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null
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
    // Coalesce concurrent callers and throttle to at most one fetch per 500ms.
    // Initial loads (empty items) bypass the throttle so first render always fetches.
    if (this.inFlight) return this.inFlight
    const isInitial = this.items.size === 0
    if (!isInitial) {
      const sinceLast = Date.now() - this.lastLoadAt
      if (sinceLast < 500) return
    }
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
  }
}
