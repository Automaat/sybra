import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'
import { Step, StepConfig, StepType } from '../../../bindings/github.com/Automaat/sybra/internal/workflow/models.js'

const StepConfigPanel = (await import('./StepConfigPanel.svelte')).default

function makeStep(overrides: Partial<Step> = {}): Step {
  return new Step({
    id: 'step-1',
    name: 'Run agent',
    type: StepType.StepRunAgent,
    config: new StepConfig({ model: 'sonnet', prompt: 'do something' }),
    ...overrides,
  })
}

describe('StepConfigPanel', () => {
  afterEach(() => { cleanup() })

  it('renders Fable 5 model option', () => {
    render(StepConfigPanel, {
      props: { step: makeStep(), allStepIds: [], onupdate: vi.fn(), ondelete: vi.fn() },
    })
    expect(screen.getByText('Fable 5')).toBeDefined()
  })

  it('renders Opus 4.8 model option', () => {
    render(StepConfigPanel, {
      props: { step: makeStep(), allStepIds: [], onupdate: vi.fn(), ondelete: vi.fn() },
    })
    expect(screen.getByText('Opus 4.8')).toBeDefined()
  })

  it('renders model select with fable value option', () => {
    render(StepConfigPanel, {
      props: { step: makeStep(), allStepIds: [], onupdate: vi.fn(), ondelete: vi.fn() },
    })
    const selects = screen.getAllByRole('combobox') as HTMLSelectElement[]
    const allValues = selects.flatMap((s) => Array.from(s.options).map((o) => o.value))
    expect(allValues).toContain('fable')
  })
})
