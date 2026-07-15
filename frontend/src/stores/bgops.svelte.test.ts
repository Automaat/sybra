import { describe, it, expect, vi, beforeEach } from 'vitest'
import * as ev from '../lib/events.js'
import type { Operation } from './bgops.svelte.js'

const mockListBackgroundOps = vi.fn()
let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('$lib/api', () => ({
  ListBackgroundOps: (...args: unknown[]) => mockListBackgroundOps(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
}))

const { bgopStore } = await import('./bgops.svelte.js')

function makeOp(overrides: Record<string, unknown> = {}): Operation {
  return {
    id: 'op-1',
    type: 'clone',
    label: 'Cloning repo',
    status: 'running',
    startedAt: new Date().toISOString(),
    ...overrides,
  }
}

describe('BgOpStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    eventCallbacks = {}
    bgopStore.ops = []
  })

  describe('load', () => {
    it('fetches operations from backend', async () => {
      const ops = [makeOp({ id: 'op1' }), makeOp({ id: 'op2', status: 'done' })]
      mockListBackgroundOps.mockResolvedValue(ops)

      await bgopStore.load()

      expect(bgopStore.ops).toHaveLength(2)
    })

    it('handles null result', async () => {
      mockListBackgroundOps.mockResolvedValue(null)

      await bgopStore.load()

      expect(bgopStore.ops).toHaveLength(0)
    })
  })

  describe('activeCount', () => {
    it('counts running operations', () => {
      bgopStore.ops = [
        makeOp({ id: '1', status: 'running' }),
        makeOp({ id: '2', status: 'done' }),
        makeOp({ id: '3', status: 'running' }),
      ]

      expect(bgopStore.activeCount).toBe(2)
    })

    it('returns 0 when no running ops', () => {
      bgopStore.ops = [makeOp({ status: 'done' })]
      expect(bgopStore.activeCount).toBe(0)
    })
  })

  describe('hasActive', () => {
    it('returns true when any op is running', () => {
      bgopStore.ops = [makeOp({ status: 'running' })]
      expect(bgopStore.hasActive).toBe(true)
    })

    it('returns false when no running ops', () => {
      bgopStore.ops = [makeOp({ status: 'done' })]
      expect(bgopStore.hasActive).toBe(false)
    })

    it('returns false when empty', () => {
      bgopStore.ops = []
      expect(bgopStore.hasActive).toBe(false)
    })
  })

  describe('listen', () => {
    it('registers all event listeners', () => {
      const unsub = bgopStore.listen()

      expect(eventCallbacks[ev.BgOpStarted]).toBeDefined()
      expect(eventCallbacks[ev.BgOpProgress]).toBeDefined()
      expect(eventCallbacks[ev.BgOpCompleted]).toBeDefined()
      expect(eventCallbacks[ev.BgOpFailed]).toBeDefined()
      unsub()
    })

    it('prepends new op on BgOpStarted', () => {
      bgopStore.ops = [makeOp({ id: 'existing' })]
      const unsub = bgopStore.listen()

      eventCallbacks[ev.BgOpStarted](makeOp({ id: 'new' }))

      expect(bgopStore.ops[0].id).toBe('new')
      expect(bgopStore.ops).toHaveLength(2)
      unsub()
    })

    it('deduplicates on BgOpStarted (replaces existing same id)', () => {
      bgopStore.ops = [makeOp({ id: 'op-1', status: 'running' })]
      const unsub = bgopStore.listen()

      eventCallbacks[ev.BgOpStarted](makeOp({ id: 'op-1', label: 'Updated label' }))

      expect(bgopStore.ops).toHaveLength(1)
      expect(bgopStore.ops[0].label).toBe('Updated label')
      unsub()
    })

    it('updates op in place on BgOpProgress', () => {
      bgopStore.ops = [makeOp({ id: 'op-1', phase: 'init' })]
      const unsub = bgopStore.listen()

      eventCallbacks[ev.BgOpProgress](makeOp({ id: 'op-1', phase: 'cloning' }))

      expect(bgopStore.ops[0].phase).toBe('cloning')
      unsub()
    })

    it('updates op on BgOpCompleted', () => {
      bgopStore.ops = [makeOp({ id: 'op-1', status: 'running' })]
      const unsub = bgopStore.listen()

      eventCallbacks[ev.BgOpCompleted](makeOp({ id: 'op-1', status: 'done' }))

      expect(bgopStore.ops[0].status).toBe('done')
      unsub()
    })

    it('updates op on BgOpFailed', () => {
      bgopStore.ops = [makeOp({ id: 'op-1', status: 'running' })]
      const unsub = bgopStore.listen()

      eventCallbacks[ev.BgOpFailed](makeOp({ id: 'op-1', status: 'failed', error: 'git error' }))

      expect(bgopStore.ops[0].status).toBe('failed')
      expect(bgopStore.ops[0].error).toBe('git error')
      unsub()
    })

    it('unsubscribes all listeners', () => {
      const unsub = bgopStore.listen()
      unsub()

      expect(eventCallbacks[ev.BgOpStarted]).toBeUndefined()
      expect(eventCallbacks[ev.BgOpProgress]).toBeUndefined()
      expect(eventCallbacks[ev.BgOpCompleted]).toBeUndefined()
      expect(eventCallbacks[ev.BgOpFailed]).toBeUndefined()
    })
  })
})
