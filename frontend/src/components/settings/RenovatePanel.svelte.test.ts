import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const RenovatePanel = (await import('./RenovatePanel.svelte')).default

function buildSettings(enabled: boolean) {
  return {
    renovate: { enabled, author: 'app/renovate' },
  } as never
}

describe('RenovatePanel', () => {
  afterEach(cleanup)

  it('disabled hides author input', () => {
    render(RenovatePanel, { props: { settings: buildSettings(false) } })
    expect(screen.queryByLabelText('PR Author')).toBeNull()
  })

  it('enabled reveals author input pre-filled', () => {
    render(RenovatePanel, { props: { settings: buildSettings(true) } })
    const input = screen.getByLabelText('PR Author') as HTMLInputElement
    expect(input.value).toBe('app/renovate')
  })

  it('enable toggle reflects current settings.enabled', () => {
    render(RenovatePanel, { props: { settings: buildSettings(false) } })
    expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(false)
    cleanup()
    render(RenovatePanel, { props: { settings: buildSettings(true) } })
    expect((screen.getAllByRole('checkbox')[0] as HTMLInputElement).checked).toBe(true)
  })
})
