import { beforeEach, describe, expect, it, vi } from 'vitest'

class MockEventSource {
  static instances: MockEventSource[] = []

  url: string
  addEventListener = vi.fn()
  removeEventListener = vi.fn()
  close = vi.fn()

  constructor(url: string) {
    this.url = url
    MockEventSource.instances.push(this)
  }
}

describe('api-http live updates auth', () => {
  beforeEach(() => {
    localStorage.clear()
    MockEventSource.instances = []
    vi.restoreAllMocks()
    vi.stubGlobal('EventSource', MockEventSource)
    vi.resetModules()
  })

  it('prompts for a token before opening the first SSE connection', async () => {
    vi.spyOn(window, 'prompt').mockReturnValue('secret')
    const { EventsOn } = await import('./api-http.ts')

    const off = EventsOn('task:updated', vi.fn())

    expect(window.prompt).toHaveBeenCalledTimes(1)
    expect(MockEventSource.instances).toHaveLength(1)
    expect(MockEventSource.instances[0]?.url).toBe('/events?token=secret')
    expect(localStorage.getItem('sybra.apiToken')).toBe('secret')

    off()
  })

  it('fails closed when the token prompt is canceled', async () => {
    vi.spyOn(window, 'prompt').mockReturnValue('')
    const { EventsOn } = await import('./api-http.ts')

    expect(() => EventsOn('task:updated', vi.fn())).toThrow(
      'sybra server auth token required for live updates',
    )
    expect(MockEventSource.instances).toHaveLength(0)
  })
})
