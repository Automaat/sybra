import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { workflow } from '../../../wailsjs/go/models.js'

const TriggerConfigPanel = (await import('./TriggerConfigPanel.svelte')).default

function makeTrigger(overrides: Partial<workflow.Trigger> = {}): workflow.Trigger {
  return new workflow.Trigger({
    on: 'task.created',
    conditions: [],
    ...overrides,
  })
}

describe('TriggerConfigPanel', () => {
  afterEach(() => { cleanup() })

  it('renders Trigger heading', () => {
    render(TriggerConfigPanel, { props: { trigger: null, onupdate: vi.fn() } })
    expect(screen.getByText('Trigger')).toBeDefined()
  })

  it('shows trigger event select', () => {
    render(TriggerConfigPanel, { props: { trigger: makeTrigger(), onupdate: vi.fn() } })
    expect(screen.getByRole('combobox')).toBeDefined()
  })

  it('shows trigger event options', () => {
    render(TriggerConfigPanel, { props: { trigger: makeTrigger(), onupdate: vi.fn() } })
    expect(screen.getByText('Task created')).toBeDefined()
    expect(screen.getByText('Task status changed')).toBeDefined()
    expect(screen.getByText('PR event')).toBeDefined()
  })

  it('calls onupdate when trigger event changes', async () => {
    const onupdate = vi.fn()
    render(TriggerConfigPanel, { props: { trigger: makeTrigger({ on: 'task.created' }), onupdate } })
    await fireEvent.change(screen.getByRole('combobox'), { target: { value: 'pr.event' } })
    expect(onupdate).toHaveBeenCalled()
    const updated = onupdate.mock.calls[0][0] as workflow.Trigger
    expect(updated.on).toBe('pr.event')
  })

  it('shows Add condition button', () => {
    render(TriggerConfigPanel, { props: { trigger: makeTrigger(), onupdate: vi.fn() } })
    expect(screen.getByText('Add condition')).toBeDefined()
  })

  it('calls onupdate with new condition when Add condition clicked', async () => {
    const onupdate = vi.fn()
    render(TriggerConfigPanel, { props: { trigger: makeTrigger({ conditions: [] }), onupdate } })
    await fireEvent.click(screen.getByText('Add condition'))
    expect(onupdate).toHaveBeenCalled()
    const updated = onupdate.mock.calls[0][0] as workflow.Trigger
    expect(updated.conditions).toHaveLength(1)
  })

  it('renders existing conditions', () => {
    const cond = new workflow.Condition({ field: 'task.tags', operator: 'contains', value: 'bug' })
    render(TriggerConfigPanel, {
      props: { trigger: makeTrigger({ conditions: [cond] }), onupdate: vi.fn() },
    })
    const input = screen.getAllByRole('textbox')[0] as HTMLInputElement
    expect(input.value).toBe('task.tags')
  })

  it('handles null trigger gracefully', () => {
    const { container } = render(TriggerConfigPanel, { props: { trigger: null, onupdate: vi.fn() } })
    expect(container).toBeDefined()
  })
})
