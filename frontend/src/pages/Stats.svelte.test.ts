import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { StatsResponse, Summary } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'

const mockLoad = vi.fn()

const mockStatsStore = {
  data: null as StatsResponse | null,
  loading: false,
  error: '',
  load: (...args: unknown[]) => mockLoad(...args),
}

vi.mock('../stores/stats.svelte.js', () => ({
  statsStore: mockStatsStore,
}))

const Stats = (await import('./Stats.svelte')).default

function makeSummary(overrides: Record<string, unknown> = {}): Summary {
  return Summary.createFrom({
    totalCostUsd: 1.5,
    totalRuns: 10,
    avgCostPerRun: 0.15,
    avgDurationS: 360,
    totalDurationS: 3600,
    totalInputTokens: 5000,
    totalOutputTokens: 2000,
    ...overrides,
  })
}

function makeStatsData(): StatsResponse {
  const s = makeSummary()
  return StatsResponse.createFrom({
    today: s,
    thisWeek: s,
    thisMonth: s,
    allTime: s,
    byProject: [],
    byProjectType: [],
    byRole: [],
    byMode: [],
    byModel: [],
    closedTasksDaily: [],
    recentRuns: [],
  })
}

describe('Stats', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockStatsStore.data = null
    mockStatsStore.error = ''
    mockStatsStore.loading = false
  })

  afterEach(() => {
    cleanup()
  })

  it('renders the page with a refresh control', () => {
    render(Stats, { props: {} })
    expect(screen.getByText('Refresh')).toBeDefined()
  })

  it('shows period tabs', () => {
    render(Stats, { props: {} })
    expect(screen.getByText('Today')).toBeDefined()
    expect(screen.getByText('This Week')).toBeDefined()
    expect(screen.getByText('This Month')).toBeDefined()
    expect(screen.getByText('All Time')).toBeDefined()
  })

  it('shows Refresh button', () => {
    render(Stats, { props: {} })
    expect(screen.getByText('Refresh')).toBeDefined()
  })

  it('calls load on mount', () => {
    render(Stats, { props: {} })
    expect(mockLoad).toHaveBeenCalled()
  })

  it('shows error when error set', () => {
    mockStatsStore.error = 'Failed to load stats'
    render(Stats, { props: {} })
    expect(screen.getByText('Failed to load stats')).toBeDefined()
  })

  it('shows summary cards when data present', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('Total Cost')).toBeDefined()
    expect(screen.getByText('Total Runs')).toBeDefined()
    expect(screen.getByText('Avg Cost / Run')).toBeDefined()
    expect(screen.getByText('Total Duration')).toBeDefined()
    expect(screen.getByText('Tokens (In / Out)')).toBeDefined()
  })

  it('shows total cost formatted', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('$1.50')).toBeDefined()
  })

  it('shows total runs', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('10')).toBeDefined()
  })

  it('shows breakdown sections', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('By Project Type')).toBeDefined()
    expect(screen.getByText('By Project')).toBeDefined()
    expect(screen.getByText('By Role')).toBeDefined()
    expect(screen.getByText('By Mode')).toBeDefined()
    expect(screen.getByText('By Model')).toBeDefined()
  })

  it('shows no data message for empty breakdowns', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    const noDataElements = screen.getAllByText('No data')
    expect(noDataElements.length).toBeGreaterThan(0)
  })

  it('shows Recent Runs section when data present', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('Recent Runs')).toBeDefined()
  })

  it('shows no runs message when recentRuns empty', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('No runs recorded yet')).toBeDefined()
  })

  it('formats duration in hours', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('1.0h')).toBeDefined()
  })

  it('formats tokens', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getByText('5.0K / 2.0K')).toBeDefined()
  })

  it('formats tokens in millions for >=1M', () => {
    const s = makeSummary({ totalInputTokens: 2_500_000, totalOutputTokens: 1_200_000 })
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('2.5M / 1.2M')).toBeDefined()
  })

  it('formats tokens raw for <1K', () => {
    const s = makeSummary({ totalInputTokens: 500, totalOutputTokens: 200 })
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('500 / 200')).toBeDefined()
  })

  it('formats duration in seconds for <60s', () => {
    const s = makeSummary({ totalDurationS: 45 })
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('45s')).toBeDefined()
  })

  it('formats duration in minutes for <3600s', () => {
    const s = makeSummary({ totalDurationS: 600 })
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('10.0m')).toBeDefined()
  })

  it('shows reasoning tokens when set', () => {
    const s = makeSummary({ totalReasoningTokens: 1500 })
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('1.5K reasoning')).toBeDefined()
  })

  it('switches period when tab clicked', async () => {
    const today = makeSummary({ totalCostUsd: 0.5, totalRuns: 2 })
    const allTime = makeSummary({ totalCostUsd: 50, totalRuns: 200 })
    mockStatsStore.data = StatsResponse.createFrom({
      today, thisWeek: allTime, thisMonth: allTime, allTime,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('$50.00')).toBeDefined()
    await fireEvent.click(screen.getByText('Today'))
    await vi.waitFor(() => {
      expect(screen.getByText('$0.50')).toBeDefined()
    })
  })

  it('clicking Refresh calls statsStore.load', async () => {
    render(Stats, { props: {} })
    mockLoad.mockClear()
    await fireEvent.click(screen.getByText('Refresh'))
    expect(mockLoad).toHaveBeenCalled()
  })

  it('renders breakdown rows when data has entries', () => {
    const s = makeSummary()
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [],
      byProjectType: [],
      byRole: [
        { key: 'triage', stats: makeSummary({ totalRuns: 5, totalCostUsd: 0.25, totalDurationS: 100 }) },
        { key: 'plan', stats: makeSummary({ totalRuns: 3, totalCostUsd: 0.5, totalDurationS: 200 }) },
      ],
      byMode: [],
      byModel: [],
      closedTasksDaily: [],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('triage')).toBeDefined()
    expect(screen.getByText('plan')).toBeDefined()
  })

  it('renders recent runs rows', () => {
    const s = makeSummary()
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [
        {
          id: 'r1',
          taskId: 'task-1',
          role: 'triage',
          mode: 'headless',
          model: 'sonnet',
          costUsd: 0.05,
          durationS: 60,
          reasoningTokens: 100,
          timestamp: '2026-05-01T10:00:00Z',
          outcome: 'completed',
        },
        {
          id: 'r2',
          taskId: 'task-2',
          role: 'eval',
          mode: 'interactive',
          model: '',
          costUsd: 0.1,
          durationS: 120,
          reasoningTokens: 0,
          timestamp: '2026-05-01T11:00:00Z',
          outcome: 'failed',
        },
      ],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('task-1')).toBeDefined()
    expect(screen.getByText('task-2')).toBeDefined()
    expect(screen.getByText('triage')).toBeDefined()
    expect(screen.getByText('eval')).toBeDefined()
    expect(screen.getByText('completed')).toBeDefined()
    expect(screen.getByText('failed')).toBeDefined()
  })

  it('renders cost-over-time and cost-by-project charts', () => {
    const s = makeSummary()
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [
        { date: '2026-05-01', count: 1 },
        { date: '2026-05-02', count: 2 },
      ],
      recentRuns: [
        { id: 'r1', taskId: 't1', projectId: 'org/repo', role: 'plan', mode: 'headless', model: 'm', costUsd: 1.5, durationS: 1, reasoningTokens: 0, timestamp: '2026-05-01T10:00:00Z', outcome: 'completed' },
        { id: 'r2', taskId: 't2', projectId: 'org/other', role: 'plan', mode: 'headless', model: 'm', costUsd: 0.5, durationS: 1, reasoningTokens: 0, timestamp: '2026-05-01T11:00:00Z', outcome: 'completed' },
      ],
    })
    render(Stats, { props: {} })
    expect(screen.getByText('Cost over time')).toBeDefined()
    expect(screen.getByText('Cost by project')).toBeDefined()
    expect(screen.getByText('Closed tasks over time')).toBeDefined()
    expect(screen.getByRole('img', { name: 'Cost over time' })).toBeDefined()
    expect(screen.getByRole('img', { name: 'Closed tasks over time' })).toBeDefined()
    // Bar chart renders both project labels (default period is All Time).
    expect(screen.getByText('org/repo')).toBeDefined()
    expect(screen.getByText('org/other')).toBeDefined()
  })

  it('uses task-specific empty labels', () => {
    mockStatsStore.data = makeStatsData()
    render(Stats, { props: {} })
    expect(screen.getAllByText('No cost in this range').length).toBeGreaterThan(0)
    expect(screen.getByText('No closed tasks in this range')).toBeDefined()
  })

  it('closed-task chart follows period switching', async () => {
    const s = makeSummary()
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [{ date: '2026-05-01', count: 1 }],
      recentRuns: [],
    })
    render(Stats, { props: {} })
    expect(screen.getByRole('img', { name: 'Closed tasks over time' })).toBeDefined()
    await fireEvent.click(screen.getByText('Today'))
    await vi.waitFor(() => {
      expect(screen.getByText('No closed tasks in this range')).toBeDefined()
    })
  })

  it('flags the sample cap on the charts when there are more than 50 runs', () => {
    const s = makeSummary()
    const runs = Array.from({ length: 50 }, (_, i) => ({
      id: `r${i}`, taskId: `t${i}`, projectId: 'org/repo', role: 'plan', mode: 'headless',
      model: 'm', costUsd: 0.1, durationS: 1, reasoningTokens: 0,
      timestamp: '2026-05-01T10:00:00Z', outcome: 'completed',
    }))
    mockStatsStore.data = StatsResponse.createFrom({
      // The backend caps recentRuns only when total runs exceeds 50.
      today: s, thisWeek: s, thisMonth: s, allTime: makeSummary({ totalRuns: 60 }),
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [{ date: '2026-05-01', count: 1 }],
      recentRuns: runs,
    })
    render(Stats, { props: {} })
    expect(screen.getAllByText(/recent 50 runs/)).toHaveLength(2)
  })

  it('shows dash for missing model in recent runs', () => {
    const s = makeSummary()
    mockStatsStore.data = StatsResponse.createFrom({
      today: s, thisWeek: s, thisMonth: s, allTime: s,
      byProject: [], byProjectType: [], byRole: [], byMode: [], byModel: [],
      closedTasksDaily: [],
      recentRuns: [
        {
          id: 'r1',
          taskId: 'task-1',
          role: 'triage',
          mode: 'headless',
          model: '',
          costUsd: 0,
          durationS: 0,
          reasoningTokens: 0,
          timestamp: '2026-05-01T10:00:00Z',
          outcome: 'completed',
        },
      ],
    })
    render(Stats, { props: {} })
    const dashes = screen.getAllByText('—')
    expect(dashes.length).toBeGreaterThan(0)
  })
})
