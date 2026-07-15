import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/svelte'

const AgentPanel = (await import('./AgentPanel.svelte')).default

function buildSettings() {
  return {
    agent: {
      provider: 'claude',
      model: 'sonnet',
      mode: 'headless',
      maxConcurrent: 3,
      requirePermissions: null,
      surviveRestart: null,
      fallbackModel: '',
      maxCostUsd: 0,
      maxTurns: 0,
      bashTimeoutSeconds: 0,
      retryWatchdog: 0,
      dispatchJitterMs: 0,
      logRetentionDays: 14,
      logGzipAfterDays: 3,
      logRetentionMaxSizeMb: 1024,
      approvalPort: 0,
      headlessPermissionMode: '',
    },
  } as never
}

describe('AgentPanel', () => {
  it('renders detected runtimes with installed, missing, and probe-error states', () => {
    render(AgentPanel, {
      props: {
        settings: buildSettings(),
        defaults: buildSettings(),
        modelOptions: [{ value: 'sonnet', label: 'Sonnet' }],
        runtimes: [
          { id: 'claude', name: 'Claude Code', installed: true, path: '/tmp/claude', version: 'Claude 1.2.3' },
          { id: 'codex', name: 'Codex', installed: false, path: '', version: '', error: '' },
          { id: 'opencode', name: 'OpenCode', installed: true, path: '/tmp/opencode', version: '', error: 'version probe timed out after 1.5s' },
          { id: 'hermes', name: 'Hermes', installed: true, path: '/tmp/hermes', version: 'Hermes 0.8.0', informationalOnly: true },
        ],
      },
    })

    expect(screen.getByText('Detected runtimes')).toBeDefined()
    expect(screen.getByText('Claude Code')).toBeDefined()
    expect(screen.getAllByText('Codex').length).toBeGreaterThan(0)
    expect(screen.getByText('Hermes')).toBeDefined()
    expect(screen.getAllByText('installed').length).toBeGreaterThan(0)
    expect(screen.getByText('missing')).toBeDefined()
    expect(screen.getByText('Version: Claude 1.2.3')).toBeDefined()
    expect(screen.getByText('Not found on PATH')).toBeDefined()
    expect(screen.getByText('Probe: version probe timed out after 1.5s')).toBeDefined()
    expect(screen.getByText('info only')).toBeDefined()
  })
})
