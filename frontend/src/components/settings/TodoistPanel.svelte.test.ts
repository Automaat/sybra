import { describe, it, expect, afterEach, vi } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const TodoistPanel = (await import('./TodoistPanel.svelte')).default

function buildSettings(enabled: boolean) {
  return {
    todoist: { enabled, apiToken: '', projectId: '', pollSeconds: 60 },
    todoistTokenSet: false,
  } as never
}

function props(enabled: boolean) {
  return {
    settings: buildSettings(enabled),
    defaults: buildSettings(true),
    onsavetoken: vi.fn(async () => {}),
  } as never
}

describe('TodoistPanel', () => {
  afterEach(cleanup)

  it('disabled hides credential inputs', () => {
    render(TodoistPanel, { props: props(false) })
    expect(screen.queryByLabelText('API token')).toBeNull()
    expect(screen.queryByLabelText('Project ID')).toBeNull()
  })

  it('enabled reveals credential inputs', () => {
    render(TodoistPanel, { props: props(true) })
    expect(screen.getByLabelText('API token')).toBeDefined()
    expect(screen.getByLabelText('Project ID')).toBeDefined()
    expect(screen.getByLabelText('Poll interval (seconds)')).toBeDefined()
  })

  it('enable toggle reflects current settings.enabled', () => {
    render(TodoistPanel, { props: props(false) })
    expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(false)
    cleanup()
    render(TodoistPanel, { props: props(true) })
    expect((screen.getAllByRole('checkbox')[0] as HTMLInputElement).checked).toBe(true)
  })
})
