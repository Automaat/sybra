import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'
import AutonomyTrendSection from './AutonomyTrendSection.svelte'
import type { AutonomySnapshot, AutonomyTrend, AutonomyWeekPoint } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

function snapshot(overrides: Partial<AutonomySnapshot>): AutonomySnapshot {
  return {
    since: '2026-01-01T00:00:00Z',
    until: '2026-02-01T00:00:00Z',
    tasksLanded: 0,
    autonomousLandings: 0,
    autonomyRate: 0,
    ...overrides,
  } as AutonomySnapshot
}

function weekPoint(overrides: Partial<AutonomyWeekPoint>): AutonomyWeekPoint {
  return {
    weekStart: '2026-01-01T00:00:00Z',
    weekEnd: '2026-01-08T00:00:00Z',
    tasksLanded: 0,
    autonomousLandings: 0,
    autonomyRate: 0,
    ...overrides,
  } as AutonomyWeekPoint
}

function trend(overrides: Partial<AutonomyTrend>): AutonomyTrend {
  return {
    generatedAt: '2026-02-01T00:00:00Z',
    overall: snapshot({ tasksLanded: 10, autonomousLandings: 8, autonomyRate: 0.8 }),
    lastMonth: snapshot({ tasksLanded: 4, autonomousLandings: 3, autonomyRate: 0.75 }),
    lastWeek: snapshot({ tasksLanded: 1, autonomousLandings: 1, autonomyRate: 1 }),
    weekly: [weekPoint({ tasksLanded: 1, autonomousLandings: 1, autonomyRate: 1 })],
    ...overrides,
  } as AutonomyTrend
}

afterEach(() => {
  cleanup()
})

describe('AutonomyTrendSection', () => {
  it('renders nothing when trend is null', () => {
    const { container } = render(AutonomyTrendSection, { props: { trend: null } })
    expect(container.textContent).toBe('')
  })

  it('renders nothing when overall has no landed tasks (fresh install)', () => {
    const { container } = render(AutonomyTrendSection, {
      props: { trend: trend({ overall: snapshot({ tasksLanded: 0, autonomousLandings: 0, autonomyRate: 0 }) }) },
    })
    expect(container.textContent).toBe('')
  })

  it('renders overall / last-month / last-week tiles with rates and landed counts', () => {
    render(AutonomyTrendSection, { props: { trend: trend({}) } })

    expect(screen.getByText('Overall')).toBeDefined()
    expect(screen.getByText('Last 30 days')).toBeDefined()
    expect(screen.getByText('Last 7 days')).toBeDefined()
    expect(screen.getByText('80%')).toBeDefined()
    expect(screen.getByText('8/10 landed')).toBeDefined()
    expect(screen.getByText('75%')).toBeDefined()
    expect(screen.getByText('3/4 landed')).toBeDefined()
    expect(screen.getByText('100%')).toBeDefined()
    expect(screen.getByText('1/1 landed')).toBeDefined()
  })

  it('renders the weekly chart with an accessible label', () => {
    render(AutonomyTrendSection, { props: { trend: trend({}) } })

    expect(screen.getByRole('img', { name: 'Autonomy rate by week' })).toBeDefined()
  })
})
