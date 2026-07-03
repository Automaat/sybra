import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const LoggingPanel = (await import('./LoggingPanel.svelte')).default

function buildSettings() {
  return {
    logging: { level: 'info', maxSizeMB: 50, maxFiles: 5 },
    audit: { enabled: true, retentionDays: 30 },
  } as never
}

function props() {
  return { settings: buildSettings(), defaults: buildSettings() } as never
}

describe('LoggingPanel', () => {
  afterEach(cleanup)

  it('renders the four numeric/select inputs', () => {
    render(LoggingPanel, { props: props() })
    expect(screen.getByLabelText('Log level')).toBeDefined()
    expect(screen.getByLabelText('Max log size (MB)')).toBeDefined()
    expect(screen.getByLabelText('Max log files')).toBeDefined()
    expect(screen.getByLabelText('Audit retention (days)')).toBeDefined()
  })

  it('renders audit toggle reflecting settings', () => {
    render(LoggingPanel, { props: props() })
    const cb = screen.getByLabelText('Enable audit logging') as HTMLInputElement
    expect(cb.checked).toBe(true)
  })

  it('changing log level updates two-way bound value', async () => {
    render(LoggingPanel, { props: props() })
    const select = screen.getByLabelText('Log level') as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'debug' } })
    expect(select.value).toBe('debug')
  })
})
