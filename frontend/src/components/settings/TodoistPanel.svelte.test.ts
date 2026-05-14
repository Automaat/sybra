import { describe, it, expect, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const TodoistPanel = (await import('./TodoistPanel.svelte')).default

function buildSettings(enabled: boolean) {
  return {
    todoist: { enabled, apiToken: '', projectId: '', pollSeconds: 60 },
  } as never
}

describe('TodoistPanel', () => {
  afterEach(cleanup)

  it('disabled hides credential inputs', () => {
    render(TodoistPanel, { props: { settings: buildSettings(false) } })
    expect(screen.queryByLabelText('API Token')).toBeNull()
    expect(screen.queryByLabelText('Project ID')).toBeNull()
  })

  it('enabled reveals credential inputs', () => {
    render(TodoistPanel, { props: { settings: buildSettings(true) } })
    expect(screen.getByLabelText('API Token')).toBeDefined()
    expect(screen.getByLabelText('Project ID')).toBeDefined()
    expect(screen.getByLabelText('Poll Interval (seconds)')).toBeDefined()
  })

  it('enable toggle reflects current settings.enabled', () => {
    render(TodoistPanel, { props: { settings: buildSettings(false) } })
    expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(false)
    cleanup()
    render(TodoistPanel, { props: { settings: buildSettings(true) } })
    expect((screen.getAllByRole('checkbox')[0] as HTMLInputElement).checked).toBe(true)
  })
})
