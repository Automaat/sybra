import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// A fake EventSource driven by hand, so the tests exercise the real state
// machine rather than a stub of it. jsdom has no EventSource at all.
class FakeEventSource {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  static instances: FakeEventSource[] = []

  readyState = FakeEventSource.CONNECTING
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  listeners = new Map<string, Set<(e: MessageEvent) => void>>()
  closed = false

  constructor(public url: string) {
    FakeEventSource.instances.push(this)
  }

  addEventListener(name: string, handler: (e: MessageEvent) => void): void {
    let set = this.listeners.get(name)
    if (!set) {
      set = new Set()
      this.listeners.set(name, set)
    }
    set.add(handler)
  }

  removeEventListener(name: string, handler: (e: MessageEvent) => void): void {
    this.listeners.get(name)?.delete(handler)
  }

  close(): void {
    this.closed = true
    this.readyState = FakeEventSource.CLOSED
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN
    this.onopen?.()
  }

  /** A transport drop: the spec puts the stream back to CONNECTING first. */
  drop(): void {
    this.readyState = FakeEventSource.CONNECTING
    this.onerror?.()
  }

  /** A fatal error (e.g. 401): EventSource gives up and never retries. */
  fail(): void {
    this.readyState = FakeEventSource.CLOSED
    this.onerror?.()
  }

  emit(name: string, data: unknown): void {
    for (const handler of this.listeners.get(name) ?? []) {
      handler({ data: JSON.stringify(data) } as MessageEvent)
    }
  }
}

let api: typeof import('./api-http.js')

beforeEach(async () => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.stubGlobal('localStorage', {
    getItem: () => 'test-token',
    setItem: () => {},
  })
  vi.resetModules()
  api = await import('./api-http.js')
})

afterEach(() => {
  api.resetEventStreamForTest()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('event stream connection state', () => {
  it('reports a drop after a successful open and a reconnect on the way back', () => {
    const seen: Array<[string, boolean]> = []
    api.OnConnectionChange((state, reconnected) => seen.push([state, reconnected]))
    api.EventsOn('task:updated', () => {})
    const es = FakeEventSource.instances[0]!

    es.open()
    // The spec reconnects on its own, so readyState is CONNECTING — not CLOSED
    // — when the error fires. Requiring CLOSED here is what made the reconnect
    // signal unreachable for an ordinary server restart.
    es.drop()
    es.open()

    expect(seen).toEqual([['open', false], ['lost', false], ['open', true]])
    expect(api.getConnectionState()).toBe('open')
  })

  it('does not claim a reconnect on the first open', () => {
    const seen: Array<[string, boolean]> = []
    api.OnConnectionChange((state, reconnected) => seen.push([state, reconnected]))
    api.EventsOn('task:updated', () => {})

    FakeEventSource.instances[0]!.open()

    expect(seen).toEqual([['open', false]])
  })

  it('rebuilds a stream EventSource abandoned, carrying subscriptions over', () => {
    vi.useFakeTimers()
    const received: unknown[] = []
    api.EventsOn('task:updated', (payload) => received.push(payload))
    const first = FakeEventSource.instances[0]!

    first.open()
    // A fatal error leaves the stream CLOSED for good; without a rebuild the UI
    // stays dark until a manual reload even once the server is back.
    first.fail()
    expect(api.getConnectionState()).toBe('lost')

    vi.advanceTimersByTime(5_000)
    expect(FakeEventSource.instances).toHaveLength(2)
    const second = FakeEventSource.instances[1]!
    expect(first.closed).toBe(true)

    second.open()
    second.emit('task:updated', 'task-abc')
    expect(received).toEqual(['task-abc'])
    expect(api.getConnectionState()).toBe('open')
  })

  it('stops rebuilding once the last subscriber is gone', () => {
    vi.useFakeTimers()
    const stop = api.EventsOn('task:updated', () => {})
    const first = FakeEventSource.instances[0]!

    first.open()
    stop()
    expect(first.closed).toBe(true)

    vi.advanceTimersByTime(30_000)
    expect(FakeEventSource.instances).toHaveLength(1)
  })
})
