import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockGetSettings = vi.fn()
const mockGetDefaultSettings = vi.fn()
const mockUpdateSettings = vi.fn()
const mockUpdateTodoistToken = vi.fn()
const mockGetRawConfig = vi.fn()
const mockSaveRawConfig = vi.fn()
const mockGetVersion = vi.fn()
const mockGetCodexModels = vi.fn()
const mockGetCopilotModels = vi.fn()
const mockProviderHealthEnabled = vi.fn()
const mockGetProviderHealth = vi.fn()
const mockSetProviderAutoFailover = vi.fn()
const mockSetProviderEnabled = vi.fn()
const mockEventsOn = vi.fn((..._args: any[]) => vi.fn())

vi.mock('$lib/api', () => ({
  GetSettings: (...args: unknown[]) => mockGetSettings(...args),
  GetDefaultSettings: (...args: unknown[]) => mockGetDefaultSettings(...args),
  UpdateSettings: (...args: unknown[]) => mockUpdateSettings(...args),
  UpdateTodoistToken: (...args: unknown[]) => mockUpdateTodoistToken(...args),
  GetRawConfig: (...args: unknown[]) => mockGetRawConfig(...args),
  SaveRawConfig: (...args: unknown[]) => mockSaveRawConfig(...args),
  GetVersion: (...args: unknown[]) => mockGetVersion(...args),
  GetCodexModels: (...args: unknown[]) => mockGetCodexModels(...args),
  GetCopilotModels: (...args: unknown[]) => mockGetCopilotModels(...args),
  ProviderHealthEnabled: (...args: unknown[]) => mockProviderHealthEnabled(...args),
  GetProviderHealth: (...args: unknown[]) => mockGetProviderHealth(...args),
  SetProviderAutoFailover: (...args: unknown[]) => mockSetProviderAutoFailover(...args),
  SetProviderEnabled: (...args: unknown[]) => mockSetProviderEnabled(...args),
  EventsOn: (...args: any[]) => mockEventsOn(...args),
}))

const Settings = (await import('./Settings.svelte')).default
const { browserNotificationStore } = await import('../lib/web-notifications.svelte.js')

function installFakeNotification(permission: NotificationPermission) {
  class FakeNotification {
    static permission = permission
    static requestPermission = vi.fn(async () => FakeNotification.permission)
  }
  ;(window as unknown as { Notification: unknown }).Notification = FakeNotification
  return FakeNotification
}

function uninstallNotification() {
  delete (window as unknown as { Notification?: unknown }).Notification
}

function baseSettings() {
  return {
    agent: {
      provider: 'claude',
      model: 'sonnet',
      mode: 'headless',
      maxConcurrent: 3,
      logRetentionDays: 14,
      logGzipAfterDays: 3,
      logRetentionMaxSizeMb: 1024,
    },
    notification: { desktop: false },
    orchestrator: { dispatchIntervalSeconds: 10, maintenanceIntervalSeconds: 60 },
    logging: { level: 'info', maxSizeMB: 100, maxFiles: 10 },
    audit: { retentionDays: 30, enabled: true },
    todoist: { enabled: false, apiToken: '', projectId: '', pollSeconds: 300 },
    renovate: { enabled: false, author: 'app/renovate' },
    providers: {
      claude: { enabled: true },
      codex: { enabled: false },
      copilot: { enabled: true },
      autoFailover: false,
      healthCheck: { intervalSeconds: 30 },
    },
    github: { enabled: false, app: {} },
    monitor: { enabled: false },
    selfMonitor: { enabled: false },
    triage: { enabled: false },
    umbrella: { enabled: false },
    testing: { maxConcurrent: 0, maxAttempts: 0 },
    experience: { enabled: false, maxRecords: 0 },
    metrics: { enabled: false },
    browser: { inApp: null },
    projectTypes: [],
    todoistTokenSet: false,
    directories: {
      tasks: '/home/.sybra/tasks',
      skills: '/home/.sybra/skills',
    },
  }
}

// The settings page is a left-rail of tabbed panes; jump to one by clicking its
// rail entry, then the pane mounts.
async function goTo(name: string) {
  await fireEvent.click(screen.getByRole('button', { name }))
}

describe('Settings', () => {
  beforeEach(() => {
    mockGetSettings.mockReset()
    mockGetDefaultSettings.mockReset()
    mockGetDefaultSettings.mockResolvedValue(baseSettings())
    mockUpdateSettings.mockReset()
    mockUpdateTodoistToken.mockResolvedValue(undefined)
    mockGetRawConfig.mockResolvedValue('agent:\n  provider: claude\n')
    mockSaveRawConfig.mockResolvedValue(undefined)
    mockGetVersion.mockResolvedValue({ server: 'v1.0.0', client: 'v1.0.0' })
    mockGetCodexModels.mockResolvedValue([])
    mockGetCopilotModels.mockResolvedValue([])
    mockProviderHealthEnabled.mockResolvedValue(false)
    mockGetProviderHealth.mockResolvedValue([])
    mockEventsOn.mockReturnValue(vi.fn())
    localStorage.clear()
    browserNotificationStore.disable()
    uninstallNotification()

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
    vi.unstubAllEnvs()
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

  it('renders the appearance section (default pane)', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    expect(screen.getByRole('heading', { name: 'Appearance' })).toBeDefined()
  })

  it('renders the section rail and clarifies save scope', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    const nav = screen.getByRole('navigation', { name: 'Settings sections' })
    expect(nav).toBeDefined()
    expect(screen.getByRole('button', { name: 'Orchestrator' })).toBeDefined()
    expect(screen.getByText(/apply instantly/)).toBeDefined()
  })

  it('filters rail entries via search', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Renovate' }))
    await fireEvent.input(screen.getByPlaceholderText('Search settings…'), { target: { value: 'renovate' } })
    await vi.waitFor(() => {
      expect(screen.getByRole('button', { name: 'Renovate' })).toBeDefined()
      expect(screen.queryByRole('button', { name: 'Orchestrator' })).toBeNull()
    })
  })

  it('keeps another section dirty when a provider toggles (no baseline wipe)', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockProviderHealthEnabled.mockResolvedValue(true)
    mockSetProviderAutoFailover.mockResolvedValue(undefined)
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    await fireEvent.click(screen.getByLabelText('Desktop notifications (macOS)'))
    expect((screen.getByText('Save') as HTMLButtonElement).disabled).toBe(false)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Providers' }))
    await goTo('Providers')
    await fireEvent.click(screen.getByLabelText(/Auto-failover between providers/))
    await vi.waitFor(() => expect(mockSetProviderAutoFailover).toHaveBeenCalled())
    expect((screen.getByText('Save') as HTMLButtonElement).disabled).toBe(false)
    expect(screen.getByText('Unsaved changes')).toBeDefined()
  })

  it('every rail entry opens a pane with a matching heading', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    const cases: [string, string][] = [
      ['Defaults', 'Agent defaults'],
      ['Notifications', 'Notifications'],
      ['Orchestrator', 'Orchestrator'],
      ['Triage & Umbrella', 'Triage'],
      ['GitHub', 'GitHub'],
      ['Monitor', 'Monitor'],
      ['Todoist', 'Todoist'],
      ['Renovate', 'Renovate'],
      ['Machine & Testing', 'Machine routing'],
      ['Logging & Audit', 'Logging & audit'],
      ['Version', 'Version'],
      ['Directories', 'Directories'],
    ]
    for (const [tab, heading] of cases) {
      await goTo(tab)
      await vi.waitFor(() => expect(screen.getByRole('heading', { name: heading })).toBeDefined())
    }
  })

  it('renders Agent Defaults pane when selected', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    await vi.waitFor(() => expect(screen.getByText('Agent defaults')).toBeDefined())
  })

  it('renders the full agent log retention controls', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/ }))
    await vi.waitFor(() => {
      expect(screen.getByLabelText('Log retention (days)')).toBeDefined()
      expect(screen.getByLabelText('Log gzip after (days)')).toBeDefined()
      expect(screen.getByLabelText('Log max size (MB)')).toBeDefined()
    })
  })

  it('renders the raw YAML config pane when selected', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    // RawConfigPanel imports GetRawConfig/SaveRawConfig directly; provide via the mock.
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Config file (YAML)' }))
    await goTo('Config file (YAML)')
    await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Config file (YAML)' })).toBeDefined())
  })

  it('Save button is disabled when settings not dirty', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    const saveBtn = screen.getByText('Save') as HTMLButtonElement
    expect(saveBtn.disabled).toBe(true)
  })

  it('calls UpdateSettings and shows success message on save', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockUpdateSettings.mockResolvedValue(undefined)
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/ }))
    const gzipAfterInput = screen.getByLabelText('Log gzip after (days)') as HTMLInputElement
    await fireEvent.input(gzipAfterInput, { target: { value: '5' } })
    await fireEvent.click(screen.getByText('Save'))
    await vi.waitFor(() => {
      expect(mockUpdateSettings).toHaveBeenCalled()
      expect(mockUpdateSettings.mock.calls.at(-1)?.[0]?.agent?.logGzipAfterDays).toBe(5)
      expect(screen.getByText('Settings saved')).toBeDefined()
    })
  })

  it('shows error message when save fails', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockUpdateSettings.mockRejectedValue(new Error('save error'))
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    const concurrencyInput = screen.getByLabelText('Max concurrent') as HTMLInputElement
    await fireEvent.input(concurrencyInput, { target: { value: '5' } })
    await fireEvent.click(screen.getByText('Save'))
    await vi.waitFor(() => {
      expect(screen.getByText('save error')).toBeDefined()
    })
  })

  it('renders Todoist pane when selected', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Todoist' }))
    await goTo('Todoist')
    await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Todoist' })).toBeDefined())
  })

  it('shows Todoist fields when Todoist enabled', async () => {
    mockGetSettings.mockResolvedValue({ ...baseSettings(), todoist: { enabled: true, apiToken: '', projectId: '', pollSeconds: 300 } })
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Todoist' }))
    await goTo('Todoist')
    await vi.waitFor(() => {
      expect(screen.getByLabelText('API token')).toBeDefined()
      expect(screen.getByLabelText('Project ID')).toBeDefined()
    })
  })

  it('renders Renovate pane when selected', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Renovate' }))
    await goTo('Renovate')
    await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Renovate' })).toBeDefined())
  })

  it('renders Version pane when selected', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Version' }))
    await goTo('Version')
    await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Version' })).toBeDefined())
  })

  it('shows Appearance pane with a Color Scheme control', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    expect(screen.getByRole('heading', { name: 'Appearance' })).toBeDefined()
    expect(screen.getByText('Color scheme')).toBeDefined()
    expect(screen.getByRole('group', { name: 'Color scheme' })).toBeDefined()
  })

  it('shows Providers pane when providerHealthEnabled is true', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockProviderHealthEnabled.mockResolvedValue(true)
    mockGetProviderHealth.mockResolvedValue([
      { provider: 'claude', healthy: true, reason: '', detail: '' },
    ])
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Providers' }))
    await goTo('Providers')
    await vi.waitFor(() => expect(screen.getByRole('heading', { name: 'Providers' })).toBeDefined())
  })

  it('hides the Providers rail entry when provider health is disabled', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockProviderHealthEnabled.mockResolvedValue(false)
    render(Settings)
    await vi.waitFor(() => expect(mockProviderHealthEnabled).toHaveBeenCalled())
    await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Defaults' })).toBeDefined())
    expect(screen.queryByRole('button', { name: 'Providers' })).toBeNull()
  })

  it('renders Color Scheme options (system, light, dark)', () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    expect(screen.getByRole('button', { name: 'System' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Light' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Dark' })).toBeDefined()
  })

  it('persists colorScheme to localStorage when changed', async () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    await fireEvent.click(screen.getByRole('button', { name: 'Dark' }))
    await vi.waitFor(() => {
      expect(localStorage.getItem('colorScheme')).toBe('dark')
    })
  })

  it('toggles dark class on html when dark selected', async () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    await fireEvent.click(screen.getByRole('button', { name: 'Dark' }))
    await vi.waitFor(() => {
      expect(document.documentElement.classList.contains('dark')).toBe(true)
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Light' }))
    await vi.waitFor(() => {
      expect(document.documentElement.classList.contains('dark')).toBe(false)
    })
  })

  it('marks the active color scheme button as pressed', async () => {
    mockGetSettings.mockReturnValue(new Promise(() => {}))
    render(Settings)
    await fireEvent.click(screen.getByRole('button', { name: 'Dark' }))
    await vi.waitFor(() => {
      expect(screen.getByRole('button', { name: 'Dark' }).getAttribute('aria-pressed')).toBe('true')
      expect(screen.getByRole('button', { name: 'Light' }).getAttribute('aria-pressed')).toBe('false')
    })
  })

  it('Reset button appears when settings are dirty', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    const concurrencyInput = screen.getByLabelText('Max concurrent') as HTMLInputElement
    await fireEvent.input(concurrencyInput, { target: { value: '7' } })
    await vi.waitFor(() => {
      expect(screen.getByText('Reset')).toBeDefined()
    })
  })

  it('Reset button reverts pending changes', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    const concurrencyInput = screen.getByLabelText('Max concurrent') as HTMLInputElement
    await fireEvent.input(concurrencyInput, { target: { value: '7' } })
    await vi.waitFor(() => screen.getByText('Reset'))
    await fireEvent.click(screen.getByText('Reset'))
    await vi.waitFor(() => {
      const after = screen.getByLabelText('Max concurrent') as HTMLInputElement
      expect(after.value).toBe('3')
    })
  })

  it('renders Save without Reset initially', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    expect(screen.getByText('Save')).toBeDefined()
    expect(screen.queryByText('Reset')).toBeNull()
  })

  it('shows server version after GetVersion resolves', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockGetVersion.mockResolvedValue({ server: 'v2.7.0', client: 'v2.7.0' })
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Version' }))
    await goTo('Version')
    await vi.waitFor(() => {
      expect(screen.getByText('v2.7.0')).toBeDefined()
    })
  })

  it('shows unavailable when GetVersion rejects', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockGetVersion.mockRejectedValue(new Error('no version'))
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Version' }))
    await goTo('Version')
    await vi.waitFor(() => {
      expect(screen.getByText('unavailable')).toBeDefined()
    })
  })

  it('renders directories list for known dir keys', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Directories' }))
    await goTo('Directories')
    await vi.waitFor(() => {
      expect(screen.getByRole('heading', { name: 'Directories' })).toBeDefined()
      expect(screen.getByDisplayValue('/home/.sybra/tasks')).toBeDefined()
      expect(screen.getByDisplayValue('/home/.sybra/skills')).toBeDefined()
    })
  })

  it('toggles desktop notifications checkbox', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    const checkbox = screen.getByLabelText('Desktop notifications (macOS)') as HTMLInputElement
    expect(checkbox.checked).toBe(false)
    await fireEvent.click(checkbox)
    await vi.waitFor(() => {
      expect(checkbox.checked).toBe(true)
    })
  })

  it('shows browser notifications as unsupported and disabled when the API is missing', async () => {
    vi.stubEnv('VITE_MODE', 'web') // browser-notification toggle is web-mode-only
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    const checkbox = screen.getByLabelText(/Browser notifications/) as HTMLInputElement
    expect(checkbox.disabled).toBe(true)
    expect(screen.getByText('Not supported in this browser')).toBeDefined()
  })

  it('enables browser notifications via a user gesture and requests permission', async () => {
    vi.stubEnv('VITE_MODE', 'web') // browser-notification toggle is web-mode-only
    const FakeNotification = installFakeNotification('default')
    // Real browsers flip Notification.permission once the user responds to
    // the prompt; the fake must mirror that or the derived status text (which
    // re-reads `permission`, not just our own `enabled` flag) never updates.
    FakeNotification.requestPermission = vi.fn(async () => {
      FakeNotification.permission = 'granted'
      return FakeNotification.permission
    })
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    const checkbox = screen.getByLabelText(/Browser notifications/) as HTMLInputElement
    expect(checkbox.checked).toBe(false)

    await fireEvent.click(checkbox)

    expect(FakeNotification.requestPermission).toHaveBeenCalledTimes(1)
    await vi.waitFor(() => {
      expect(checkbox.checked).toBe(true)
      expect(screen.getByText(/live Sybra notifications will show/)).toBeDefined()
    })
  })

  it('does not prompt again once permission was previously denied', async () => {
    vi.stubEnv('VITE_MODE', 'web') // browser-notification toggle is web-mode-only
    installFakeNotification('denied')
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    const checkbox = screen.getByLabelText(/Browser notifications/) as HTMLInputElement
    expect(checkbox.disabled).toBe(true)
    expect(screen.getByText(/Blocked/)).toBeDefined()
  })

  it('unchecking browser notifications disables the preference without re-prompting', async () => {
    vi.stubEnv('VITE_MODE', 'web') // browser-notification toggle is web-mode-only
    const FakeNotification = installFakeNotification('granted')
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    const checkbox = screen.getByLabelText(/Browser notifications/) as HTMLInputElement
    await fireEvent.click(checkbox)
    await vi.waitFor(() => expect(checkbox.checked).toBe(true))

    await fireEvent.click(checkbox)

    await vi.waitFor(() => expect(checkbox.checked).toBe(false))
    expect(FakeNotification.requestPermission).not.toHaveBeenCalled()
  })

  it('hides the browser-notification toggle in desktop mode', async () => {
    installFakeNotification('granted') // API present, but desktop must not expose it
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Notifications' }))
    await goTo('Notifications')
    // Native macOS toggle stays; the browser toggle is web-only.
    expect(screen.getByLabelText(/Desktop notifications/)).toBeDefined()
    expect(screen.queryByLabelText(/Browser notifications/)).toBeNull()
  })

  it('edits orchestrator dispatch interval', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Orchestrator' }))
    await goTo('Orchestrator')
    await fireEvent.click(screen.getByText('Loop cadence'))
    const input = screen.getByLabelText('Dispatch interval (seconds)') as HTMLInputElement
    expect(input.value).toBe('10')
    await fireEvent.input(input, { target: { value: '20' } })
    await vi.waitFor(() => {
      expect(input.value).toBe('20')
    })
  })

  it('renders Fable 5 and Opus 4.8 model options for claude provider', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    expect(screen.getByText('Fable 5')).toBeDefined()
    expect(screen.getByText('Opus 4.8')).toBeDefined()
  })

  it('switches model options when provider changes to codex', async () => {
    mockGetSettings.mockResolvedValue(baseSettings())
    mockGetCodexModels.mockResolvedValue([
      { slug: 'gpt-5.4', display_name: 'GPT-5.4' },
    ])
    render(Settings)
    await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
    await goTo('Defaults')
    const providerSelect = screen.getByLabelText('Agent type') as HTMLSelectElement
    await fireEvent.change(providerSelect, { target: { value: 'codex' } })
    await vi.waitFor(() => {
      expect(screen.getByText('GPT-5.4')).toBeDefined()
    })
  })

  it('clears success message after save', async () => {
    vi.useFakeTimers()
    try {
      mockGetSettings.mockResolvedValue(baseSettings())
      mockUpdateSettings.mockResolvedValue(undefined)
      render(Settings)
      await vi.waitFor(() => screen.getByRole('button', { name: 'Defaults' }))
      await goTo('Defaults')
      const concurrencyInput = screen.getByLabelText('Max concurrent') as HTMLInputElement
      await fireEvent.input(concurrencyInput, { target: { value: '5' } })
      await fireEvent.click(screen.getByText('Save'))
      await vi.waitFor(() => screen.getByText('Settings saved'))
      vi.advanceTimersByTime(3001)
      await vi.waitFor(() => {
        expect(screen.queryByText('Settings saved')).toBeNull()
      })
    } finally {
      vi.useRealTimers()
    }
  })
})
