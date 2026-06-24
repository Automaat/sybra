import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockEnabled = vi.fn()
const mockGetHealth = vi.fn()
const mockSetEnabled = vi.fn()
const mockSetAutoFailover = vi.fn()
const mockEventsOn = vi.fn()

vi.mock('$lib/api', () => ({
  EventsOn: (...args: unknown[]) => mockEventsOn(...args),
  ProviderHealthEnabled: () => mockEnabled(),
  GetProviderHealth: () => mockGetHealth(),
  SetProviderEnabled: (...args: unknown[]) => mockSetEnabled(...args),
  SetProviderAutoFailover: (...args: unknown[]) => mockSetAutoFailover(...args),
}))

const ProviderHealthPanel = (await import('./ProviderHealthPanel.svelte')).default

function buildSettings() {
  return {
    providers: {
      claude: { enabled: true },
      codex: { enabled: false },
      autoFailover: false,
      healthCheck: { intervalSeconds: 30 },
    },
  } as never
}

describe('ProviderHealthPanel', () => {
  beforeEach(() => {
    mockEnabled.mockReset()
    mockGetHealth.mockReset()
    mockSetEnabled.mockReset()
    mockSetAutoFailover.mockReset()
    mockEventsOn.mockReset()
    mockEventsOn.mockReturnValue(() => {})
    mockEnabled.mockResolvedValue(true)
    mockGetHealth.mockResolvedValue([
      { provider: 'claude', healthy: true, reason: '' },
      { provider: 'codex', healthy: false, reason: 'rate-limited' },
    ])
    mockSetEnabled.mockResolvedValue(undefined)
    mockSetAutoFailover.mockResolvedValue(undefined)
  })
  afterEach(cleanup)

  it('renders nothing when provider health is disabled at runtime', async () => {
    mockEnabled.mockResolvedValue(false)
    const { container } = render(ProviderHealthPanel, {
      props: { settings: buildSettings(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(container.querySelector('h2')).toBeNull()
    })
  })

  it('renders Providers section with both rows when enabled', async () => {
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(screen.getByText('Providers')).toBeDefined()
      expect(screen.getByText('claude')).toBeDefined()
      expect(screen.getByText('codex')).toBeDefined()
    })
  })

  it('shows healthy badge for claude and rate-limited reason for codex', async () => {
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(screen.getByText('healthy')).toBeDefined()
      expect(screen.getByText('rate-limited')).toBeDefined()
    })
  })

  it('toggling auto-failover calls SetProviderAutoFailover + onsettingschange', async () => {
    const onsettingschange = vi.fn()
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), onsettingschange },
    })
    await waitFor(() => screen.getByText('Providers'))
    const cb = screen.getByLabelText(/Auto-failover/) as HTMLInputElement
    await fireEvent.click(cb)
    await waitFor(() => {
      expect(mockSetAutoFailover).toHaveBeenCalledWith(true)
      expect(onsettingschange).toHaveBeenCalled()
    })
  })

  it('subscribes to ProviderHealth events on mount', async () => {
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalled()
    })
  })
})
