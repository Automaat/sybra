import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockGetHealth = vi.fn()
const mockSetEnabled = vi.fn()
const mockSetAutoFailover = vi.fn()
const mockEventsOn = vi.fn()

vi.mock('$lib/api', () => ({
  EventsOn: (...args: unknown[]) => mockEventsOn(...args),
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
      copilot: { enabled: true },
      opencode: { enabled: true },
      autoFailover: false,
      healthCheck: { intervalSeconds: 30 },
    },
  } as never
}

function buildRuntimes() {
  return [
    { id: 'claude', name: 'Claude Code', installed: true, path: '/tmp/claude', version: 'Claude 1.2.3' },
    { id: 'codex', name: 'Codex', installed: false, path: '', version: '', error: '' },
    { id: 'opencode', name: 'OpenCode', installed: true, path: '/tmp/opencode', version: '', error: 'version probe timed out after 1.5s' },
    { id: 'hermes', name: 'Hermes', installed: true, path: '/tmp/hermes', version: 'Hermes 0.8.0', informationalOnly: true },
  ]
}

describe('ProviderHealthPanel', () => {
  beforeEach(() => {
    mockGetHealth.mockReset()
    mockSetEnabled.mockReset()
    mockSetAutoFailover.mockReset()
    mockEventsOn.mockReset()
    mockEventsOn.mockReturnValue(() => {})
    mockGetHealth.mockResolvedValue([
      { provider: 'claude', healthy: true, reason: '' },
      { provider: 'codex', healthy: false, reason: 'rate-limited' },
    ])
    mockSetEnabled.mockResolvedValue(undefined)
    mockSetAutoFailover.mockResolvedValue(undefined)
  })
  afterEach(cleanup)

  it('renders nothing when provider health is disabled', async () => {
    const { container } = render(ProviderHealthPanel, {
      props: { settings: buildSettings(), enabled: false, runtimes: buildRuntimes(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(container.querySelector('h2')).toBeNull()
    })
  })

  it('renders Providers section with both rows when enabled', async () => {
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), enabled: true, runtimes: buildRuntimes(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(screen.getByText('Providers')).toBeDefined()
      expect(screen.getByText('claude')).toBeDefined()
      expect(screen.getByText('codex')).toBeDefined()
    })
  })

  it('shows healthy badge for claude and rate-limited reason for codex', async () => {
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), enabled: true, runtimes: buildRuntimes(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(screen.getByText('healthy')).toBeDefined()
      expect(screen.getByText('rate-limited')).toBeDefined()
    })
  })

  it('toggling auto-failover calls SetProviderAutoFailover + onsettingschange', async () => {
    const onsettingschange = vi.fn()
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), enabled: true, runtimes: buildRuntimes(), onsettingschange },
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
      props: { settings: buildSettings(), enabled: true, runtimes: buildRuntimes(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(mockEventsOn).toHaveBeenCalled()
    })
  })

  it('shows runtime path/version beside provider rows and Hermes as informational-only', async () => {
    render(ProviderHealthPanel, {
      props: { settings: buildSettings(), enabled: true, runtimes: buildRuntimes(), onsettingschange: vi.fn() },
    })
    await waitFor(() => {
      expect(screen.getByText('/tmp/claude')).toBeDefined()
      expect(screen.getByText('version: Claude 1.2.3')).toBeDefined()
      expect(screen.getByText('CLI not found on PATH')).toBeDefined()
      expect(screen.getByText('probe: version probe timed out after 1.5s')).toBeDefined()
      expect(screen.getByText('Hermes')).toBeDefined()
      expect(screen.getByText('informational only')).toBeDefined()
      expect(screen.getByText('No provider toggle')).toBeDefined()
    })
  })
})
