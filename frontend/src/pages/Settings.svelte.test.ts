import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockGetSettings = vi.fn()
const mockUpdateSettings = vi.fn()
const mockGetVersion = vi.fn()
const mockProviderHealthEnabled = vi.fn()
const mockGetProviderHealth = vi.fn()
const mockSetProviderAutoFailover = vi.fn()
const mockSetProviderEnabled = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())

vi.mock('$lib/api', () => ({
  GetSettings: (...args: unknown[]) => mockGetSettings(...args),
  UpdateSettings: (...args: unknown[]) => mockUpdateSettings(...args),
  GetVersion: (...args: unknown[]) => mockGetVersion(...args),
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

vi.mock('../../bindings/github.com/Automaat/sybra/internal/sybra/integrationservice.js', () => ({
  GetProviderHealth: (...args: unknown[]) => mockGetProviderHealth(...args),
  ProviderHealthEnabled: (...args: unknown[]) => mockProviderHealthEnabled(...args),
  SetProviderAutoFailover: (...args: unknown[]) => mockSetProviderAutoFailover(...args),
  SetProviderEnabled: (...args: unknown[]) => mockSetProviderEnabled(...args),
}))

const Settings = (await import('./Settings.svelte')).default

const mockSettings = {
  agent: { provider: 'claude', model: 'sonnet', mode: 'headless', maxConcurrent: 3 },
  notification: { desktop: false },
  orchestrator: { autoTriage: false, autoPlan: false },
  logging: { level: 'info', maxSizeMB: 100, maxFiles: 10 },
  audit: { retentionDays: 30, enabled: true },
  todoist: { enabled: false, apiToken: '', projectId: '', pollSeconds: 300 },
  renovate: { enabled: false, author: 'app/renovate' },
  providers: {
    claude: { enabled: true },
    codex: { enabled: false },
    autoFailover: false,
    healthCheck: { intervalSeconds: 30 },
  },
  directories: {
    tasks: '/home/.sybra/tasks',
    skills: '/home/.sybra/skills',
  },
}

describe('Settings', () => {
  beforeEach(() => {
    mockGetSettings.mockReset()
    mockUpdateSettings.mockReset()
    mockGetVersion.mockResolvedValue({ server: 'v1.0.0', client: 'v1.0.0' })
    mockProviderHealthEnabled.mockResolvedValue(false)
    mockGetProviderHealth.mockResolvedValue([])
    mockEventsOn.mockReturnValue(vi.fn())

    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    })
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('shows loading state while GetSettings is pending', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    expect(screen.getByText('Loading…')).toBeDefined()
  })

  it('shows error when GetSettings fails', async () => {
    mockGetSettings.mockRejectedValue(new Error('backend unavailable'))
    render(Settings)
    await vi.waitFor(() => {
      const errors = screen.getAllByText('Error: backend unavailable')
      expect(errors.length).toBeGreaterThan(0)
    })
  })

  it('renders Settings heading', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    expect(screen.getByText('Settings')).toBeDefined()
  })

  it('renders Agent Defaults section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Agent Defaults')).toBeDefined()
    })
  })

  it('renders Notifications section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Notifications')).toBeDefined()
    })
  })

  it('renders Orchestrator section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Orchestrator')).toBeDefined()
    })
  })

  it('renders Logging & Audit section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Logging & Audit')).toBeDefined()
    })
  })

  it('renders Directories section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Directories')).toBeDefined()
    })
  })

  it('Save button is disabled when settings not dirty', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Agent Defaults')).toBeDefined()
    })
    const saveBtn = screen.getByText('Save') as HTMLButtonElement
    expect(saveBtn.disabled).toBe(true)
  })

  it('calls UpdateSettings and shows success message on save', async () => {
    mockGetSettings.mockResolvedValue({ ...mockSettings })
    mockUpdateSettings.mockResolvedValue(undefined)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Max Concurrent')).toBeDefined()
    })
    // Make settings dirty by changing a field
    const concurrencyInput = screen.getByLabelText('Max Concurrent') as HTMLInputElement
    await fireEvent.input(concurrencyInput, { target: { value: '5' } })
    const saveBtn = screen.getByText('Save') as HTMLButtonElement
    await fireEvent.click(saveBtn)
    await vi.waitFor(() => {
      expect(mockUpdateSettings).toHaveBeenCalled()
      expect(screen.getByText('Settings saved')).toBeDefined()
    })
  })

  it('shows error message when save fails', async () => {
    mockGetSettings.mockResolvedValue({ ...mockSettings })
    mockUpdateSettings.mockRejectedValue(new Error('save error'))
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Max Concurrent')).toBeDefined()
    })
    const concurrencyInput = screen.getByLabelText('Max Concurrent') as HTMLInputElement
    await fireEvent.input(concurrencyInput, { target: { value: '5' } })
    const saveBtn = screen.getByText('Save') as HTMLButtonElement
    await fireEvent.click(saveBtn)
    await vi.waitFor(() => {
      expect(screen.getByText('Error: save error')).toBeDefined()
    })
  })

  it('renders Todoist section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Todoist')).toBeDefined()
    })
  })

  it('shows Todoist fields when Todoist enabled', async () => {
    mockGetSettings.mockResolvedValue({ ...mockSettings, todoist: { enabled: true, apiToken: '', projectId: '', pollSeconds: 300 } })
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByLabelText('API Token')).toBeDefined()
      expect(screen.getByLabelText('Project ID')).toBeDefined()
    })
  })

  it('renders Renovate section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Renovate')).toBeDefined()
    })
  })

  it('renders Version section after load', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Version')).toBeDefined()
    })
  })

  it('shows Appearance section with Color Scheme select', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    expect(screen.getByText('Appearance')).toBeDefined()
    expect(screen.getByLabelText('Color Scheme')).toBeDefined()
  })

  it('shows Providers section when providerHealthEnabled is true', async () => {
    mockGetSettings.mockResolvedValue(mockSettings)
    mockProviderHealthEnabled.mockResolvedValue(true)
    mockGetProviderHealth.mockResolvedValue([
      { provider: 'claude', healthy: true, reason: '', detail: '' },
    ])
    render(Settings)
    await vi.waitFor(() => {
      expect(screen.getByText('Providers')).toBeDefined()
    })
  })
})
