import { describe, it, expect, vi, beforeEach, beforeAll } from 'vitest'
import { render, cleanup } from '@testing-library/svelte'

const mockLoad = vi.fn()
const mockAgentLoad = vi.fn()
const mockStartPolling = vi.fn()
const mockStopPolling = vi.fn()
const mockEventsOn = vi.fn(() => vi.fn())

vi.mock('../wailsjs/runtime/runtime.js', () => ({
  EventsOn: (...args: unknown[]) => mockEventsOn(...args),
}))

vi.mock('./stores/tasks.svelte.js', () => ({
  taskStore: {
    load: (...args: unknown[]) => mockLoad(...args),
    loading: false,
    error: '',
    list: [],
    byStatus: () => [],
  },
}))

vi.mock('./stores/agents.svelte.js', () => ({
  agentStore: {
    load: (...args: unknown[]) => mockAgentLoad(...args),
    startPolling: (...args: unknown[]) => mockStartPolling(...args),
    stopPolling: (...args: unknown[]) => mockStopPolling(...args),
    loading: false,
    error: '',
    list: [],
    byState: () => [],
    agents: new Map(),
  },
}))

vi.mock('@skeletonlabs/skeleton-svelte', () => ({
  Navigation: Object.assign(() => {}, {
    Header: () => {},
    Content: () => {},
    Trigger: () => {},
    TriggerText: () => {},
  }),
  AppBar: Object.assign(() => {}, {
    Toolbar: () => {},
    Lead: () => {},
    Trail: () => {},
  }),
  SegmentedControl: Object.assign(() => {}, {
    Control: () => {},
    Indicator: () => {},
    Item: Object.assign(() => {}, {
      ItemText: () => {},
      ItemHiddenInput: () => {},
    }),
  }),
  Dialog: Object.assign(() => {}, {
    Backdrop: () => {},
    Positioner: () => {},
    Content: () => {},
    Title: () => {},
    CloseTrigger: () => {},
  }),
}))

const App = (await import('./App.svelte')).default

beforeAll(() => {
  if (!globalThis.ResizeObserver) {
    globalThis.ResizeObserver = class {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
  }
})

describe('App', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    cleanup()
  })

  it('renders without errors', () => {
    render(App)
  })

  it('loads tasks and agents on mount', async () => {
    render(App)
    await new Promise((r) => setTimeout(r, 0))

    expect(mockLoad).toHaveBeenCalled()
    expect(mockAgentLoad).toHaveBeenCalled()
  })

  it('starts agent polling on mount', async () => {
    render(App)
    await new Promise((r) => setTimeout(r, 0))

    expect(mockStartPolling).toHaveBeenCalled()
  })

  it('subscribes to task events', async () => {
    render(App)
    await new Promise((r) => setTimeout(r, 0))

    const eventNames = mockEventsOn.mock.calls.map((c: unknown[]) => c[0])
    expect(eventNames).toContain('task:created')
    expect(eventNames).toContain('task:updated')
    expect(eventNames).toContain('task:deleted')
  })
})
