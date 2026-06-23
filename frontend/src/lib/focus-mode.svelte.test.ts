import { describe, it, expect, beforeEach } from 'vitest'
import { focusModeStore } from './focus-mode.svelte.js'

describe('focusModeStore', () => {
  beforeEach(() => {
    localStorage.clear()
    focusModeStore.set(false)
  })

  it('defaults to disabled', () => {
    expect(focusModeStore.enabled).toBe(false)
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
