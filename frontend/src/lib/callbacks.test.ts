import { describe, it, expect, vi } from 'vitest'

const mockPushLocal = vi.fn()

vi.mock('../stores/notifications.svelte.js', () => ({
  notificationStore: {
    pushLocal: (...args: unknown[]) => mockPushLocal(...args),
  },
}))

const { guard } = await import('./callbacks.js')

describe('guard', () => {
  it('calls wrapped function with arguments', async () => {
    const fn = vi.fn().mockResolvedValue(undefined)
    const wrapped = guard(fn)
    await wrapped('a', 'b')
    expect(fn).toHaveBeenCalledWith('a', 'b')
  })

  it('does not call notificationStore on success', async () => {
    const fn = vi.fn().mockResolvedValue(undefined)
    const wrapped = guard(fn)
    await wrapped()
    expect(mockPushLocal).not.toHaveBeenCalled()
  })

  it('calls notificationStore.pushLocal on error', async () => {
    const fn = vi.fn().mockRejectedValue(new Error('api failure'))
    const wrapped = guard(fn)
    await wrapped()
    expect(mockPushLocal).toHaveBeenCalledWith('error', 'Action failed', 'Error: api failure')
  })

  it('uses custom title in error notification', async () => {
    const fn = vi.fn().mockRejectedValue(new Error('oops'))
    const wrapped = guard(fn, 'Custom error')
    await wrapped()
    expect(mockPushLocal).toHaveBeenCalledWith('error', 'Custom error', 'Error: oops')
  })

  it('handles synchronous throw', async () => {
    const fn = vi.fn().mockImplementation(() => { throw new Error('sync error') })
    const wrapped = guard(fn)
    await wrapped()
    expect(mockPushLocal).toHaveBeenCalledWith('error', 'Action failed', 'Error: sync error')
  })

  it('returns a function that returns a Promise', () => {
    const fn = vi.fn().mockResolvedValue(42)
    const wrapped = guard(fn)
    expect(wrapped()).toBeInstanceOf(Promise)
  })
})
