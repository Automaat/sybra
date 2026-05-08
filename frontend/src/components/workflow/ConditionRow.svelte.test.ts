import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { workflow } from '../../../wailsjs/go/models.js'

const ConditionRow = (await import('./ConditionRow.svelte')).default

function makeCond(overrides: Partial<workflow.Condition> = {}): workflow.Condition {
  return new workflow.Condition({
    field: 'task.status',
    operator: 'equals',
    value: 'done',
    ...overrides,
  })
}

describe('ConditionRow', () => {
  afterEach(() => { cleanup() })

  it('renders field input with condition value', () => {
    render(ConditionRow, { props: { condition: makeCond({ field: 'task.tags' }), onupdate: vi.fn() } })
    const input = screen.getByPlaceholderText('task.tags')
    expect((input as HTMLInputElement).value).toBe('task.tags')
  })

  it('renders operator select with current value', () => {
    render(ConditionRow, { props: { condition: makeCond({ operator: 'contains' }), onupdate: vi.fn() } })
    const sel = screen.getByRole('combobox') as HTMLSelectElement
    expect(sel.value).toBe('contains')
  })

  it('shows all operator options', () => {
    render(ConditionRow, { props: { condition: makeCond(), onupdate: vi.fn() } })
    expect(screen.getByText('equals')).toBeDefined()
    expect(screen.getByText('not_equals')).toBeDefined()
    expect(screen.getByText('contains')).toBeDefined()
    expect(screen.getByText('not_contains')).toBeDefined()
    expect(screen.getByText('exists')).toBeDefined()
  })

  it('shows value input when operator is not exists', () => {
    render(ConditionRow, { props: { condition: makeCond({ operator: 'equals' }), onupdate: vi.fn() } })
    expect(screen.getByPlaceholderText('value')).toBeDefined()
  })

  it('hides value input when operator is exists', () => {
    render(ConditionRow, { props: { condition: makeCond({ operator: 'exists' }), onupdate: vi.fn() } })
    expect(screen.queryByPlaceholderText('value')).toBeNull()
  })

  it('calls onupdate when field changes', async () => {
    const onupdate = vi.fn()
    render(ConditionRow, { props: { condition: makeCond({ field: 'old' }), onupdate } })
    const input = screen.getAllByRole('textbox')[0]
    await fireEvent.change(input, { target: { value: 'new.field' } })
    expect(onupdate).toHaveBeenCalled()
    const updated = onupdate.mock.calls[0][0] as workflow.Condition
    expect(updated.field).toBe('new.field')
  })

  it('calls onupdate when operator changes', async () => {
    const onupdate = vi.fn()
    render(ConditionRow, { props: { condition: makeCond({ operator: 'equals' }), onupdate } })
    await fireEvent.change(screen.getByRole('combobox'), { target: { value: 'contains' } })
    expect(onupdate).toHaveBeenCalled()
    const updated = onupdate.mock.calls[0][0] as workflow.Condition
    expect(updated.operator).toBe('contains')
  })

  it('shows remove button when onremove provided', () => {
    render(ConditionRow, { props: { condition: makeCond(), onupdate: vi.fn(), onremove: vi.fn() } })
    expect(screen.getByRole('button', { name: 'Remove condition' })).toBeDefined()
  })

  it('does not show remove button when onremove not provided', () => {
    render(ConditionRow, { props: { condition: makeCond(), onupdate: vi.fn() } })
    expect(screen.queryByRole('button', { name: 'Remove condition' })).toBeNull()
  })

  it('calls onremove when remove button clicked', async () => {
    const onremove = vi.fn()
    render(ConditionRow, { props: { condition: makeCond(), onupdate: vi.fn(), onremove } })
    await fireEvent.click(screen.getByRole('button', { name: 'Remove condition' }))
    expect(onremove).toHaveBeenCalled()
  })

  it('uses custom fieldPlaceholder', () => {
    render(ConditionRow, {
      props: { condition: makeCond({ field: '' }), onupdate: vi.fn(), fieldPlaceholder: 'pr.title' },
    })
    expect(screen.getByPlaceholderText('pr.title')).toBeDefined()
  })
})
