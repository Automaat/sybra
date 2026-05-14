import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

const LoggingPanel = (await import('./LoggingPanel.svelte')).default

function buildSettings() {
  return {
    logging: { level: 'info', maxSizeMB: 50, maxFiles: 5 },
    audit: { enabled: true, retentionDays: 30 },
  } as never
}

describe('LoggingPanel', () => {
  afterEach(cleanup)

  it('renders the four numeric/select inputs', () => {
    render(LoggingPanel, { props: { settings: buildSettings() } })
    expect(screen.getByLabelText('Log Level')).toBeDefined()
    expect(screen.getByLabelText('Max Log Size (MB)')).toBeDefined()
    expect(screen.getByLabelText('Max Log Files')).toBeDefined()
    expect(screen.getByLabelText('Audit Retention (days)')).toBeDefined()
  })

  it('renders audit toggle reflecting settings', () => {
    render(LoggingPanel, { props: { settings: buildSettings() } })
    const cb = screen.getByLabelText('Enable audit logging') as HTMLInputElement
    expect(cb.checked).toBe(true)
  })

  it('changing log level updates two-way bound value', async () => {
    const s = buildSettings()
    render(LoggingPanel, { props: { settings: s } })
    const select = screen.getByLabelText('Log Level') as HTMLSelectElement
    await fireEvent.change(select, { target: { value: 'debug' } })
    expect(select.value).toBe('debug')
  })
})
