import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const AppWarningsBanner = (await import('./AppWarningsBanner.svelte')).default

const baseProps = {
  online: true,
  networkOnline: true,
  unhealthyProviders: [],
  degradedWarnings: [],
  ondismissDegraded: vi.fn(),
} as const

describe('AppWarningsBanner', () => {
  afterEach(cleanup)

  it('renders nothing when online + healthy + no warnings', () => {
    const { container } = render(AppWarningsBanner, { props: { ...baseProps, ondismissDegraded: vi.fn() } as never })
    expect(container.textContent?.trim()).toBe('')
  })

  it('shows offline banner with "Backend unreachable" when network up', () => {
    render(AppWarningsBanner, {
      props: { ...baseProps, online: false, networkOnline: true, ondismissDegraded: vi.fn() } as never,
    })
    expect(screen.getByText('Offline')).toBeDefined()
    expect(screen.getByText(/Backend unreachable/)).toBeDefined()
  })

  it('shows offline banner with "No network connection" when network down', () => {
    render(AppWarningsBanner, {
      props: { ...baseProps, online: false, networkOnline: false, ondismissDegraded: vi.fn() } as never,
    })
    expect(screen.getByText(/No network connection/)).toBeDefined()
  })

  it('lists each unhealthy provider with reason', () => {
    render(AppWarningsBanner, {
      props: {
        ...baseProps,
        unhealthyProviders: [
          { provider: 'claude', healthy: false, reason: 'rate-limited' } as never,
          { provider: 'codex', healthy: false, reason: 'timeout' } as never,
        ],
        ondismissDegraded: vi.fn(),
      } as never,
    })
    expect(screen.getByText('claude')).toBeDefined()
    expect(screen.getByText(/rate-limited/)).toBeDefined()
    expect(screen.getByText('codex')).toBeDefined()
    expect(screen.getByText(/timeout/)).toBeDefined()
  })

  it('shows "failing over to peer" when failoverActive', () => {
    render(AppWarningsBanner, {
      props: {
        ...baseProps,
        unhealthyProviders: [{ provider: 'claude', healthy: false, reason: 'rate-limited', failoverActive: true } as never],
        ondismissDegraded: vi.fn(),
      } as never,
    })
    expect(screen.getByText(/failing over to peer/)).toBeDefined()
  })

  it('lists degraded warnings with dismiss button calling callback with index', async () => {
    const ondismissDegraded = vi.fn()
    render(AppWarningsBanner, {
      props: {
        ...baseProps,
        degradedWarnings: [
          { subsystem: 'github', reason: 'rate-limited' },
          { subsystem: 'todoist', reason: 'auth-failed' },
        ],
        ondismissDegraded,
      } as never,
    })
    expect(screen.getByText('github')).toBeDefined()
    expect(screen.getByText('todoist')).toBeDefined()
    const buttons = screen.getAllByLabelText('Dismiss')
    await fireEvent.click(buttons[1])
    expect(ondismissDegraded).toHaveBeenCalledWith(1)
  })

  it('renders repeated umbrella degraded warnings independently and dismisses by index', async () => {
    const ondismissDegraded = vi.fn()
    render(AppWarningsBanner, {
      props: {
        ...baseProps,
        degradedWarnings: [
          { subsystem: 'umbrella', reason: 'https://github.com/o/r/issues/1 degraded' },
          { subsystem: 'umbrella', reason: 'https://github.com/o/r/issues/2 degraded' },
        ],
        ondismissDegraded,
      } as never,
    })
    expect(screen.getByText(/issues\/1 degraded/)).toBeDefined()
    expect(screen.getByText(/issues\/2 degraded/)).toBeDefined()
    const buttons = screen.getAllByLabelText('Dismiss')
    expect(buttons).toHaveLength(2)
    await fireEvent.click(buttons[1])
    expect(ondismissDegraded).toHaveBeenCalledWith(1)
    // The first warning survives — only its dismiss handler should have been
    // called with a different index if clicked separately.
    expect(screen.getByText(/issues\/1 degraded/)).toBeDefined()
  })
})
