import { cleanup, render, screen, within } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mockEvaluationStore = {
  data: null as Record<string, unknown> | null,
  loading: false,
  error: '',
  load: vi.fn(),
  listen: vi.fn(),
  stopListening: vi.fn(),
}

const mockLifecycleStore = {
  data: null as Record<string, unknown> | null,
  load: vi.fn(),
}

vi.mock('../stores/evaluation.svelte.js', () => ({
  evaluationStore: mockEvaluationStore,
}))

vi.mock('../stores/lifecycle.svelte.js', () => ({
  lifecycleStore: mockLifecycleStore,
}))

const Evaluation = (await import('./Evaluation.svelte')).default

function makeReport() {
  const comparisonRow = {
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
  return {
    overall: {
      windowDays: 30,
      agentRuns: 20,
      tasksLanded: 10,
      merged: 9,
      mergedWithEdits: 1,
      closed: 0,
      autonomyRate: 0.8,
      humanTouchedLandings: 2,
      ciFirstPassRate: 0.9,
      failureRate: 0.1,
      agentFailures: 2,
      reverted: 0,
      changeFailureRate: 0,
      costPerLanded: 5,
      totalCostUsd: 50,
      reworkTasks: 1,
      leadTimeP50H: 1,
      leadTimeP90H: 2,
      cycleTimeP50H: 0.5,
      cycleTimeP90H: 1,
      turnsPerLanded: 3,
      toolsPerLanded: 10,
    },
    weaknesses: [],
    byProvider: [],
    byRole: [],
    byAgentModel: [comparisonRow],
    byExperimentKind: [
      {
        kind: 'model',
        rows: [
          {
            ...comparisonRow,
            key: 'exp-a:v1:implementation',
            experimentId: 'exp-a',
            variantId: 'v1',
            durationP90S: 1_500,
          },
        ],
        rowsContribution: [],
        experiments: [],
      },
    ],
    notes: [],
  }
}

describe('Evaluation', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockEvaluationStore.data = makeReport()
    mockEvaluationStore.loading = false
    mockEvaluationStore.error = ''
    mockLifecycleStore.data = null
  })

  afterEach(() => {
    cleanup()
  })

  it('renders A/B interpretation only for experiment rows while preserving raw metric columns', () => {
    render(Evaluation, { props: {} })

    const agentSection = screen.getByText('Agent / Model').closest('div')
    const experimentSection = screen.getByText('Model experiments').closest('div')

    expect(agentSection).not.toBeNull()
    expect(experimentSection).not.toBeNull()
    expect(within(experimentSection as HTMLElement).getByText('Promising')).toBeDefined()
    expect(within(experimentSection as HTMLElement).getByText(/Landed\/run: 50%/)).toBeDefined()
    expect(within(experimentSection as HTMLElement).getByText(/CI first pass: ok/)).toBeDefined()
    expect(within(experimentSection as HTMLElement).getByText(/Limited signals:/)).toBeDefined()
    expect(within(agentSection as HTMLElement).queryByText('Promising')).toBeNull()
    expect(within(agentSection as HTMLElement).queryByText(/Landed\/run:/)).toBeNull()
    expect(within(agentSection as HTMLElement).queryByText(/CI first pass: ok/)).toBeNull()

    for (const label of ['Runs', 'Landed', 'Merge %', 'CI 1st', 'Edited', 'Rework', 'Revert', 'Duration', 'Cost', 'Premium req']) {
      expect(within(agentSection as HTMLElement).getByText(label)).toBeDefined()
      expect(within(experimentSection as HTMLElement).getByText(label)).toBeDefined()
    }
  })

  it('shows distinct empty-state messages for unconfigured and zero-runs kinds, and hides unknown when absent', () => {
    const report = makeReport()
    report.byExperimentKind.push({ kind: 'skill', rows: [], rowsContribution: [], experiments: [] })
    mockEvaluationStore.data = report

    render(Evaluation, { props: {} })

    expect(screen.getByText('Model experiments')).toBeDefined()
    expect(screen.getByText('Prompt experiments')).toBeDefined()
    expect(screen.getByText('No prompt experiments configured.')).toBeDefined()
    expect(screen.getByText('Skill experiments')).toBeDefined()
    expect(screen.getByText('Skill experiments configured, but no runs recorded yet.')).toBeDefined()
    expect(screen.queryByText('Unknown experiments')).toBeNull()
  })
})
