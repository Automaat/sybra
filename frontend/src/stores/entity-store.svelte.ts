export class EntityStore<T extends { id: string }> {
  items = $state<Map<string, T>>(new Map())
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null

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
    // Only flip loading for the initial load so background refreshes
    // (polling + fsnotify events) don't blank out the UI.
    const isInitial = this.items.size === 0
    if (isInitial) this.loading = true
    this.error = ''
    try {
      const result = await this.loadFn()
      const map = new Map<string, T>()
      for (const item of result ?? []) map.set(item.id, item)
      this.items = map
    } catch (e) {
      this.error = String(e)
    } finally {
      if (isInitial) this.loading = false
    }
  }

  startPolling(interval = 5000): void {
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
