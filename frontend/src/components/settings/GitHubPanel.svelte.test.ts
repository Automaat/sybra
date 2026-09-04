import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const mockEventsOn = vi.fn((_event?: unknown, _cb?: unknown) => () => {})

vi.mock('$lib/api', () => ({
  EventsOn: (event: unknown, cb: unknown) => mockEventsOn(event, cb),
}))

const GitHubPanel = (await import('./GitHubPanel.svelte')).default

function buildSettings() {
  return {
    github: {
      enabled: true,
      pollerRole: '',
      nativeAutoMerge: false,
      autoResolveCleanMerges: false,
      renovateFastSeconds: 120,
      renovateSlowSeconds: 600,
      app: { enabled: false, appId: 0, installationId: 0, privateKeyPath: '' },
      polling: {
        issues: { enabled: true, intervalSeconds: 600 },
        sybraPrs: { enabled: true, activeIntervalSeconds: 120, idleIntervalSeconds: 600 },
        assignedPrs: { enabled: true, activeIntervalSeconds: 120, idleIntervalSeconds: 600 },
      },
    },
  } as never
}

describe('GitHubPanel', () => {
  beforeEach(() => {
    mockEventsOn.mockReset()
    mockEventsOn.mockReturnValue(() => {})
  })

  afterEach(cleanup)

  it('renders three independent GitHub stream toggles', async () => {
    render(GitHubPanel, { props: { settings: buildSettings(), defaults: buildSettings() } })
    await fireEvent.click(screen.getByRole('button', { name: /Poll intervals \(seconds\)/ }))
    expect(screen.getByLabelText('Enable Issues stream')).toBeDefined()
    expect(screen.getByLabelText('Enable Sybra PR stream')).toBeDefined()
    expect(screen.getByLabelText('Enable Assigned PR stream')).toBeDefined()
  })

  it('shows secondary-machine activity states for each stream', async () => {
    const settings = buildSettings() as any
    settings.github.pollerRole = 'secondary'
    render(GitHubPanel, { props: { settings, defaults: buildSettings() } as never })
    await fireEvent.click(screen.getByRole('button', { name: /Poll intervals \(seconds\)/ }))
    expect(screen.getByText('Inactive on this machine; secondary skips issue searches')).toBeDefined()
    expect(screen.getByText('Active in local-only mode; known linked task PRs still reconcile')).toBeDefined()
    expect(screen.getByText('Inactive on this machine; secondary skips assigned/reviewed searches')).toBeDefined()
  })

  it('hides polling controls when GitHub integration is disabled', () => {
    const settings = buildSettings() as any
    settings.github.enabled = false
    render(GitHubPanel, { props: { settings, defaults: buildSettings() } as never })
    expect(screen.queryByLabelText('Poller role')).toBeNull()
    expect(screen.queryByLabelText('Enable Issues stream')).toBeNull()
  })

  it('subscribes to issue and review events on mount', () => {
    render(GitHubPanel, { props: { settings: buildSettings(), defaults: buildSettings() } })
    expect(mockEventsOn).toHaveBeenCalledTimes(2)
  })
})
