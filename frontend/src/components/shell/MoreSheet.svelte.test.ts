import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockReset = vi.fn()
vi.mock('../../lib/navigation.svelte.js', () => ({
  navStore: { reset: (...a: unknown[]) => mockReset(...a) },
}))

const MoreSheet = (await import('./MoreSheet.svelte')).default

describe('MoreSheet', () => {
  afterEach(() => {
    cleanup()
    mockReset.mockClear()
  })

  it('lists Logbook (reachable on mobile) and not Reviews (it lives in the tab bar)', () => {
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    expect(screen.getByText('Logbook')).toBeDefined()
    // Reviews is a primary bottom-tab; it must not be duplicated here.
    expect(screen.queryByText('Reviews')).toBeNull()
  })

  it('navigates to the logbook when its entry is tapped', async () => {
    render(MoreSheet, { props: { open: true, onOpenChange: vi.fn() } })
    await fireEvent.click(screen.getByText('Logbook'))
    expect(mockReset).toHaveBeenCalledWith({ kind: 'logbook' })
  })
})
