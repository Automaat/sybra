import { describe, it, expect, beforeEach, vi } from 'vitest'
import { focusModeStore } from './focus-mode.svelte.js'

describe('focusModeStore', () => {
  beforeEach(() => {
    localStorage.clear()
    focusModeStore.set(false)
  })

  it('initializes disabled when storage is empty', async () => {
    vi.resetModules()
    localStorage.removeItem('focusMode')
    const { focusModeStore: fresh } = await import('./focus-mode.svelte.js')
    expect(fresh.enabled).toBe(false)
  })

  it('initializes enabled when storage says so', async () => {
    vi.resetModules()
    localStorage.setItem('focusMode', 'true')
    const { focusModeStore: fresh } = await import('./focus-mode.svelte.js')
    expect(fresh.enabled).toBe(true)
  })

  it('set() updates the flag and persists', () => {
    focusModeStore.set(true)
    expect(focusModeStore.enabled).toBe(true)
    expect(localStorage.getItem('focusMode')).toBe('true')
  })

  it('toggle() flips the flag', () => {
    expect(focusModeStore.enabled).toBe(false)
    focusModeStore.toggle()
    expect(focusModeStore.enabled).toBe(true)
    focusModeStore.toggle()
    expect(focusModeStore.enabled).toBe(false)
  })
})
