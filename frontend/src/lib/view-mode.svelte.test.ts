import { describe, it, expect, beforeEach } from 'vitest'
import { viewModeStore } from './view-mode.svelte.js'

describe('viewModeStore', () => {
  beforeEach(() => {
    localStorage.clear()
    viewModeStore.set('list')
  })

  it('cycles between the primary list and board views only', () => {
    viewModeStore.cycle()
    expect(viewModeStore.mode).toBe('board')
    viewModeStore.cycle()
    expect(viewModeStore.mode).toBe('list')
  })

  it('returns to list when cycling out of the advanced timeline view', () => {
    viewModeStore.set('timeline')
    viewModeStore.cycle()
    expect(viewModeStore.mode).toBe('list')
  })

  it('still allows setting the timeline view explicitly', () => {
    viewModeStore.set('timeline')
    expect(viewModeStore.mode).toBe('timeline')
  })
})
