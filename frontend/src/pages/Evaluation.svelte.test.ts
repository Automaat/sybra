import { cleanup, render, screen, within } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

type Subject = { workflowId?: string; stepId?: string; role?: string; skillName?: string }

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

const mockLearningStore = {
  digests: [] as Record<string, unknown>[],
  status: null as Record<string, unknown> | null,
  loading: false,
  error: '',
  load: vi.fn(),
  listen: vi.fn(),
  stopListening: vi.fn(),
}

vi.mock('../stores/evaluation.svelte.js', () => ({
  evaluationStore: mockEvaluationStore,
}))

vi.mock('../stores/learning.svelte.js', () => ({
  learningStore: mockLearningStore,
}))

vi.mock('../stores/lifecycle.svelte.js', () => ({
  lifecycleStore: mockLifecycleStore,
}))

const Evaluation = (await import('./Evaluation.svelte')).default

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

function makeReport() {
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
        groups: [
          {
            experimentId: 'exp-a',
            subject: undefined as Subject | undefined,
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
    mockLearningStore.digests = []
    mockLearningStore.status = null
    mockLearningStore.loading = false
    mockLearningStore.error = ''
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
    report.byExperimentKind.push({ kind: 'skill', groups: [] })
    mockEvaluationStore.data = report

    render(Evaluation, { props: {} })

    expect(screen.getByText('Model experiments')).toBeDefined()
    expect(screen.getByText('Prompt experiments')).toBeDefined()
    expect(screen.getByText('No prompt experiments configured.')).toBeDefined()
    expect(screen.getByText('Skill experiments')).toBeDefined()
    expect(screen.getByText('Skill experiments configured, but no runs recorded yet.')).toBeDefined()
    expect(screen.queryByText('Unknown experiments')).toBeNull()
  })

  it('renders separate tables for prompt experiments with different subjects', () => {
    const report = makeReport()
    report.byExperimentKind.push({
      kind: 'prompt',
      groups: [
        {
          experimentId: 'prompt-author',
          subject: { workflowId: 'wf-a', stepId: 'author', role: 'implementation' },
          rows: [
            { ...comparisonRow, key: 'prompt-author:p1', experimentId: 'prompt-author', variantId: 'p1' },
          ],
          rowsContribution: [],
          experiments: [],
        },
        {
          experimentId: 'prompt-review',
          subject: { workflowId: 'wf-b', stepId: 'review', role: 'review' },
          rows: [
            { ...comparisonRow, key: 'prompt-review:r1', experimentId: 'prompt-review', variantId: 'r1', provider: 'codex', model: 'gpt-5.5' },
          ],
          rowsContribution: [],
          experiments: [],
        },
      ],
    })
    mockEvaluationStore.data = report

    render(Evaluation, { props: {} })

    const promptSection = screen.getByText('Prompt experiments').closest('div') as HTMLElement
    expect(within(promptSection).getByText('prompt-author', { exact: false })).toBeDefined()
    expect(within(promptSection).getByText('prompt-review', { exact: false })).toBeDefined()
    expect(within(promptSection).getAllByRole('table')).toHaveLength(2)
  })

  it('mounts the learning digest card and cleans up the listener', () => {
    const rendered = render(Evaluation, { props: {} })

    expect(screen.getByText('Agent learning journal')).toBeDefined()
    expect(mockLearningStore.load).toHaveBeenCalledTimes(1)
    expect(mockLearningStore.listen).toHaveBeenCalledTimes(1)

    rendered.unmount()

    expect(mockLearningStore.stopListening).toHaveBeenCalledTimes(1)
  })

  it('keeps the learning card visible when evaluation data is empty', () => {
    mockEvaluationStore.data = null
    mockLearningStore.status = { enabled: false, nextRun: null }

    render(Evaluation, { props: {} })

    expect(screen.getByText('No evaluation data yet.')).toBeDefined()
    expect(
      screen.getByText('Learning digest pipeline is off. No journal entries will appear until it is enabled.'),
    ).toBeDefined()
  })
})
