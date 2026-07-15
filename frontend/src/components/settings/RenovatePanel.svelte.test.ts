import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const RenovatePanel = (await import('./RenovatePanel.svelte')).default

function buildSettings(enabled: boolean) {
  return {
    renovate: { enabled, author: 'app/renovate' },
  } as never
}

function props(enabled: boolean) {
  return { settings: buildSettings(enabled), defaults: buildSettings(true) } as never
}

describe('RenovatePanel', () => {
  afterEach(cleanup)

  it('disabled hides author input', () => {
    render(RenovatePanel, { props: props(false) })
    expect(screen.queryByLabelText('PR author')).toBeNull()
  })

  it('enabled reveals author input pre-filled', () => {
    render(RenovatePanel, { props: props(true) })
    const input = screen.getByLabelText('PR author') as HTMLInputElement
    expect(input.value).toBe('app/renovate')
  })

  it('enable toggle reflects current settings.enabled', () => {
    render(RenovatePanel, { props: props(false) })
    expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(false)
    cleanup()
    render(RenovatePanel, { props: props(true) })
    expect((screen.getAllByRole('checkbox')[0] as HTMLInputElement).checked).toBe(true)
  })
})
