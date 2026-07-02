import { cleanup, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'
import ExperimentGroup from './ExperimentGroup.svelte'
import type {
  ComparisonBreakdown,
  ExperimentKindBreakdown,
} from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

const baseRow = {
  key: 'provider:model:medium:implementation',
  provider: 'provider',
  model: 'model',
  role: 'implementation',
  reasoningEffort: 'medium',
  runs: 20,
  failures: 0,
  failureRate: 0,
  landed: 10,
  merged: 9,
  mergedWithEdits: 1,
  closed: 0,
  mergeRate: 0.9,
  mergedWithEditsRate: 0.1,
  ciFirstPassRate: 0.9,
  reworkRate: 0.1,
  revertRate: 0,
  durationP50S: 600,
  durationP90S: 1_000,
  totalCostUsd: 50,
  costPerLanded: 5,
  premiumRequests: 20,
  premiumRequestsPerLanded: 2,
  turnsPerLanded: 3,
  toolsPerLanded: 10,
  insufficientData: false,
  qualityAttributionLimited: false,
}

function row(overrides: Partial<ComparisonBreakdown>): ComparisonBreakdown {
  return { ...baseRow, ...overrides } as ComparisonBreakdown
}

function breakdown(kind: string, groups: ExperimentKindBreakdown['groups']): ExperimentKindBreakdown {
  return { kind, groups }
}

afterEach(() => {
  cleanup()
})

describe('ExperimentGroup', () => {
  it('renders the empty message when no breakdown is configured', () => {
    render(ExperimentGroup, { props: { kind: 'prompt', title: 'Prompt experiments', breakdown: undefined } })

    expect(screen.getByText('Prompt experiments')).toBeDefined()
    expect(screen.getByText('No prompt experiments configured.')).toBeDefined()
  })

  it('supports a custom emptyMessage override', () => {
    render(ExperimentGroup, {
      props: {
        kind: 'skill',
        title: 'Skill experiments',
        breakdown: undefined,
        emptyMessage: 'See #1204 for skill experiment enrollment.',
      },
    })

    expect(screen.getByText('See #1204 for skill experiment enrollment.')).toBeDefined()
  })

  it('shows a zero-runs message when configured but no groups recorded', () => {
    render(ExperimentGroup, { props: { kind: 'skill', title: 'Skill experiments', breakdown: breakdown('skill', []) } })

    expect(screen.getByText('Skill experiments configured, but no runs recorded yet.')).toBeDefined()
  })

  it('renders one table per experiment within a kind, keyed by experiment id', () => {
    render(ExperimentGroup, {
      props: {
        kind: 'prompt',
        title: 'Prompt experiments',
        breakdown: breakdown('prompt', [
          {
            experimentId: 'prompt-author',
            subject: { workflowId: 'wf-a', stepId: 'author', role: 'implementation' },
            rows: [row({ key: 'prompt-author:p1', experimentId: 'prompt-author', variantId: 'p1' })],
            rowsContribution: [],
            experiments: [],
          },
          {
            experimentId: 'prompt-review',
            subject: { workflowId: 'wf-b', stepId: 'review', role: 'review' },
            rows: [row({ key: 'prompt-review:r1', experimentId: 'prompt-review', variantId: 'r1' })],
            rowsContribution: [],
            experiments: [],
          },
        ]),
      },
    })

    expect(screen.getByText('prompt-author', { exact: false })).toBeDefined()
    expect(screen.getByText('prompt-review', { exact: false })).toBeDefined()
    expect(screen.getAllByRole('table')).toHaveLength(2)
  })

  it('marks a low-sample badge and a breached guardrail as dominant caveats', () => {
    const { container } = render(ExperimentGroup, {
      props: {
        kind: 'model',
        title: 'Model experiments',
        breakdown: breakdown('model', [
          {
            experimentId: 'exp-a',
            subject: undefined,
            rows: [
              row({
                key: 'exp-a:v1',
                experimentId: 'exp-a',
                variantId: 'v1',
                sampleStatus: 'low-sample',
                revertRate: 0.1, // breaches the revert guardrail (breachAbove: 0)
              }),
            ],
            rowsContribution: [],
            experiments: [],
          },
        ]),
      },
    })

    const dominant = container.querySelectorAll('.experiment-caveat--dominant')
    expect(dominant.length).toBeGreaterThanOrEqual(2)
    expect(within(container).getByText('low-sample').classList.contains('experiment-caveat--dominant')).toBe(true)
    expect(within(container).getByText(/Revert: breach/).classList.contains('experiment-caveat--dominant')).toBe(true)
  })

  it('never renders pause/retire affordances', () => {
    const { container } = render(ExperimentGroup, {
      props: {
        kind: 'model',
        title: 'Model experiments',
        breakdown: breakdown('model', [
          {
            experimentId: 'exp-a',
            subject: undefined,
            rows: [row({ key: 'exp-a:v1', experimentId: 'exp-a', variantId: 'v1' })],
            rowsContribution: [],
            experiments: [],
          },
        ]),
      },
    })

    expect(container.querySelectorAll('button')).toHaveLength(0)
    expect(screen.queryByText(/pause/i)).toBeNull()
    expect(screen.queryByText(/retire/i)).toBeNull()
  })
})
