import { describe, it, expect } from 'vitest'
import { periodCutoff, dailyCost, costByProject, closedTasksSeries } from './stats-charts.js'
import type { RunRecord, TaskSeriesPoint } from '../../bindings/github.com/Automaat/sybra/internal/stats/models.js'

function run(timestamp: string, costUsd: number, projectId = ''): RunRecord {
  return { timestamp, costUsd, projectId } as unknown as RunRecord
}

describe('periodCutoff', () => {
  // Wed Jun 24 2026, 15:00 local.
  const now = new Date(2026, 5, 24, 15, 0, 0)

  it('today is local midnight', () => {
    expect(periodCutoff('today', now)).toEqual(new Date(2026, 5, 24))
  })

  it('thisWeek is the most recent Sunday, on or before today', () => {
    const c = periodCutoff('thisWeek', now)!
    expect(c.getDay()).toBe(0)
    expect(c.getTime()).toBeLessThanOrEqual(new Date(2026, 5, 24).getTime())
  })

  it('thisMonth is the first of the month', () => {
    expect(periodCutoff('thisMonth', now)).toEqual(new Date(2026, 5, 1))
  })

  it('allTime has no cutoff', () => {
    expect(periodCutoff('allTime', now)).toBeNull()
  })
})

describe('dailyCost', () => {
  it('buckets by local day and sums, ascending', () => {
    const runs = [
      run('2026-06-24T10:00:00', 1.5, 'a'),
      run('2026-06-24T12:00:00', 0.5, 'a'),
      run('2026-06-23T09:00:00', 2, 'b'),
    ]
    expect(dailyCost(runs, null)).toEqual([
      { date: '2026-06-23', cost: 2 },
      { date: '2026-06-24', cost: 2 },
    ])
  })

  it('excludes runs before the cutoff', () => {
    const runs = [run('2026-06-20T10:00:00', 5, 'a'), run('2026-06-24T10:00:00', 1, 'a')]
    expect(dailyCost(runs, new Date(2026, 5, 23))).toEqual([{ date: '2026-06-24', cost: 1 }])
  })

  it('skips runs with an invalid or missing timestamp', () => {
    const runs = [
      run('not-a-date', 5, 'a'),
      { timestamp: null, costUsd: 9, projectId: 'a' } as unknown as RunRecord,
      run('2026-06-24T10:00:00', 1, 'a'),
    ]
    expect(dailyCost(runs, null)).toEqual([{ date: '2026-06-24', cost: 1 }])
  })

  it('fills zero-cost days through `end` so the line is time-proportional', () => {
    const runs = [run('2026-06-22T10:00:00', 2, 'a'), run('2026-06-24T10:00:00', 1, 'a')]
    const out = dailyCost(runs, new Date(2026, 5, 22), new Date(2026, 5, 24, 15))
    expect(out).toEqual([
      { date: '2026-06-22', cost: 2 },
      { date: '2026-06-23', cost: 0 },
      { date: '2026-06-24', cost: 1 },
    ])
  })

  it('returns empty (no fill) when there are no runs in range', () => {
    expect(dailyCost([], new Date(2026, 5, 22), new Date(2026, 5, 24))).toEqual([])
  })
})

describe('closedTasksSeries', () => {
  function taskPoint(date: string, count: number): TaskSeriesPoint {
    return { date, count } as TaskSeriesPoint
  }

  it('sorts ascending and fills zero-count days through end', () => {
    const out = closedTasksSeries(
      [taskPoint('2026-06-24', 2), taskPoint('2026-06-22', 1)],
      new Date(2026, 5, 22),
      new Date(2026, 5, 24, 15),
    )
    expect(out).toEqual([
      { date: '2026-06-22', value: 1 },
      { date: '2026-06-23', value: 0 },
      { date: '2026-06-24', value: 2 },
    ])
  })

  it('filters by cutoff and returns empty when no points remain in range', () => {
    expect(closedTasksSeries([taskPoint('2026-06-20', 5)], new Date(2026, 5, 23), new Date(2026, 5, 24))).toEqual([])
  })

  it('returns empty for nil or empty input', () => {
    expect(closedTasksSeries(null, null, new Date(2026, 5, 24))).toEqual([])
    expect(closedTasksSeries([], null, new Date(2026, 5, 24))).toEqual([])
  })

  it('parses YYYY-MM-DD as local dates instead of UTC dates', () => {
    const out = closedTasksSeries([taskPoint('2026-06-24', 1)], new Date(2026, 5, 24), new Date(2026, 5, 24, 15))
    expect(out).toEqual([{ date: '2026-06-24', value: 1 }])
  })

  it('skips invalid date keys', () => {
    expect(closedTasksSeries([taskPoint('2026-02-31', 1), taskPoint('bad', 2)], null, new Date(2026, 5, 24))).toEqual([])
  })
})

describe('costByProject', () => {
  it('groups by project, descending', () => {
    const runs = [
      run('2026-06-24T10:00:00', 3, 'a'),
      run('2026-06-24T11:00:00', 1, 'a'),
      run('2026-06-24T10:00:00', 2, 'b'),
    ]
    expect(costByProject(runs, null)).toEqual([
      { project: 'a', cost: 4 },
      { project: 'b', cost: 2 },
    ])
  })

  it('labels a missing project as (none), matching the backend', () => {
    expect(costByProject([run('2026-06-24T10:00:00', 1, '')], null)).toEqual([
      { project: '(none)', cost: 1 },
    ])
  })

  it('drops zero-cost projects and caps to topN', () => {
    const runs = Array.from({ length: 8 }, (_, i) => run('2026-06-24T10:00:00', i + 1, `p${i}`))
    const out = costByProject(runs, null, 3)
    expect(out).toHaveLength(3)
    expect(out[0]).toEqual({ project: 'p7', cost: 8 })
  })
})
