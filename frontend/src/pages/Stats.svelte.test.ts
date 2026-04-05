import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/svelte'

const mockLoad = vi.fn()

const mockStatsStore = {
  data: null as any,
  loading: false,
  error: '',
  load: (...args: unknown[]) => mockLoad(...args),
}

vi.mock('../stores/stats.svelte.js', () => ({
  statsStore: mockStatsStore,
}))

const Stats = (await import('./Stats.svelte')).default

function makeSummary(overrides: Record<string, unknown> = {}) {
  return {
    totalCostUsd: 1.5,
    totalRuns: 10,
    avgCostPerRun: 0.15,
    totalDurationS: 3600,
    totalInputTokens: 5000,
    totalOutputTokens: 2000,
    ...overrides,
  }
}

function makeStatsData() {
  const s = makeSummary()
  return {
    today: s,
    thisWeek: s,
    thisMonth: s,
    allTime: s,
    byProject: [],
    byRole: [],
    byMode: [],
    byModel: [],
    recentRuns: [],
  }
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

  it('renders Stats heading', () => {
    render(Stats, { props: {} })
    expect(screen.getByText('Stats')).toBeDefined()
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
})
