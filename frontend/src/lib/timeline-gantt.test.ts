import { describe, it, expect } from 'vitest'
import { Task, Status } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
import {
  getTaskRange,
  computeTimelineDomain,
  bucketTicks,
  taskBarPosition,
  dueDateMarkerPosition,
} from './timeline-gantt.js'

function makeTask(overrides: Partial<Task> = {}): Task {
  return Task.createFrom({
    id: 'task-1',
    title: 'Test task',
    status: Status.StatusTodo,
    taskType: '',
    agentMode: 'headless',
    allowedTools: [],
    tags: [],
    projectId: '',
    branch: '',
    prNumber: 0,
    issue: '',
    statusReason: '',
    body: '',
    plan: '',
    planCritique: '',
    slug: '',
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-15T00:00:00Z',
    dueDate: '',
    closedAt: '',
    ...overrides,
  })
}

const NOW = new Date('2026-05-07T12:00:00Z')

describe('getTaskRange', () => {
  it('returns createdAt as start', () => {
    const t = makeTask({ createdAt: '2026-01-01T00:00:00Z' })
    const { start } = getTaskRange(t, NOW)
    expect(start.getFullYear()).toBe(2026)
    expect(start.getMonth()).toBe(0) // January
    expect(start.getDate()).toBe(1)
  })

  it('uses dueDate as end when set', () => {
    const t = makeTask({ dueDate: '2026-06-01T00:00:00Z' })
    const { end } = getTaskRange(t, NOW)
    expect(end.getMonth()).toBe(5) // June
  })

  it('uses updatedAt as end for done tasks', () => {
    const t = makeTask({ status: Status.StatusDone, updatedAt: '2026-03-15T00:00:00Z' })
    const { end } = getTaskRange(t, NOW)
    expect(end.getMonth()).toBe(2) // March
    expect(end.getDate()).toBe(15)
  })

  it('uses now as end for non-done tasks without dueDate', () => {
    const t = makeTask({ status: Status.StatusInProgress })
    const { end } = getTaskRange(t, NOW)
    expect(end.getTime()).toBe(NOW.getTime())
  })

  it('ensures end is at least 1 day after start when end <= start', () => {
    const t = makeTask({
      createdAt: '2026-01-15T00:00:00Z',
      status: Status.StatusDone,
      updatedAt: '2026-01-15T00:00:00Z',
    })
    const { start, end } = getTaskRange(t, NOW)
    expect(end.getTime()).toBeGreaterThan(start.getTime())
  })
})

describe('computeTimelineDomain', () => {
  it('returns default range for empty task list', () => {
    const domain = computeTimelineDomain([], NOW)
    expect(domain.min.getTime()).toBeLessThan(NOW.getTime())
    expect(domain.max.getTime()).toBeGreaterThan(NOW.getTime())
  })

  it('includes all task ranges in domain', () => {
    const tasks = [
      makeTask({ createdAt: '2026-04-20T00:00:00Z', status: Status.StatusDone, updatedAt: '2026-04-25T00:00:00Z' }),
      makeTask({ createdAt: '2026-04-28T00:00:00Z', status: Status.StatusDone, updatedAt: '2026-05-05T00:00:00Z' }),
    ]
    const domain = computeTimelineDomain(tasks, NOW)
    expect(domain.min.getTime()).toBeLessThanOrEqual(new Date('2026-04-20T00:00:00Z').getTime())
    expect(domain.max.getTime()).toBeGreaterThanOrEqual(new Date('2026-05-05T00:00:00Z').getTime())
  })

  it('domain max is at least now+7d', () => {
    const tasks = [makeTask({ status: Status.StatusDone, updatedAt: '2026-01-02T00:00:00Z' })]
    const domain = computeTimelineDomain(tasks, NOW)
    const expectedMin = new Date(NOW.getTime() + 7 * 86_400_000)
    expect(domain.max.getTime()).toBeGreaterThanOrEqual(expectedMin.getTime())
  })
})

describe('bucketTicks', () => {
  const domain = {
    min: new Date('2026-01-01T00:00:00Z'),
    max: new Date('2026-01-10T00:00:00Z'),
  }

  it('returns empty array for zero-width domain', () => {
    const d = { min: new Date('2026-01-01'), max: new Date('2026-01-01') }
    expect(bucketTicks(d, 'day')).toEqual([])
  })

  it('generates day ticks for day zoom', () => {
    const ticks = bucketTicks(domain, 'day')
    expect(ticks.length).toBeGreaterThan(0)
    // Day labels have numeric dates
    expect(ticks[0].label).toMatch(/Jan/)
  })

  it('generates week ticks for week zoom', () => {
    const weekDomain = {
      min: new Date('2026-01-01T00:00:00Z'),
      max: new Date('2026-03-01T00:00:00Z'),
    }
    const ticks = bucketTicks(weekDomain, 'week')
    expect(ticks.length).toBeGreaterThan(0)
    expect(ticks[0].label).toMatch(/^W\d+$/)
  })

  it('generates month ticks for month zoom', () => {
    const yearDomain = {
      min: new Date('2026-01-01T00:00:00Z'),
      max: new Date('2026-12-31T00:00:00Z'),
    }
    const ticks = bucketTicks(yearDomain, 'month')
    expect(ticks.length).toBeGreaterThanOrEqual(12)
  })

  it('first tick has leftPct >= 0', () => {
    const ticks = bucketTicks(domain, 'day')
    expect(ticks[0].leftPct).toBeGreaterThanOrEqual(0)
  })
})

describe('taskBarPosition', () => {
  const domain = {
    min: new Date('2026-01-01T00:00:00Z'),
    max: new Date('2026-02-01T00:00:00Z'),
  }

  it('returns zero widths for zero-width domain', () => {
    const d = { min: new Date('2026-01-01'), max: new Date('2026-01-01') }
    const t = makeTask({ createdAt: '2026-01-01T00:00:00Z' })
    const pos = taskBarPosition(t, d, NOW)
    expect(pos.leftPct).toBe(0)
    expect(pos.widthPct).toBe(0)
  })

  it('returns non-zero width for task within domain', () => {
    const t = makeTask({
      createdAt: '2026-01-05T00:00:00Z',
      status: Status.StatusDone,
      updatedAt: '2026-01-20T00:00:00Z',
    })
    const pos = taskBarPosition(t, domain, NOW)
    expect(pos.widthPct).toBeGreaterThan(0)
    expect(pos.leftPct).toBeGreaterThan(0)
  })

  it('clamps leftPct to minimum 0', () => {
    const t = makeTask({
      createdAt: '2025-12-01T00:00:00Z',
      status: Status.StatusDone,
      updatedAt: '2026-01-05T00:00:00Z',
    })
    const pos = taskBarPosition(t, domain, NOW)
    expect(pos.leftPct).toBeGreaterThanOrEqual(0)
  })

  it('minimum widthPct is 0.5', () => {
    const t = makeTask({
      createdAt: '2026-01-15T00:00:00Z',
      status: Status.StatusDone,
      updatedAt: '2026-01-15T00:00:00Z',
    })
    const pos = taskBarPosition(t, domain, NOW)
    expect(pos.widthPct).toBeGreaterThanOrEqual(0.5)
  })
})

describe('dueDateMarkerPosition', () => {
  const domain = {
    min: new Date('2026-01-01T00:00:00Z'),
    max: new Date('2026-02-01T00:00:00Z'),
  }

  it('returns null when no dueDate', () => {
    const t = makeTask({ dueDate: '' })
    expect(dueDateMarkerPosition(t, domain)).toBeNull()
  })

  it('returns percentage for due date within domain', () => {
    const t = makeTask({ dueDate: '2026-01-16T00:00:00Z' })
    const pct = dueDateMarkerPosition(t, domain)
    expect(pct).not.toBeNull()
    expect(pct).toBeGreaterThan(0)
    expect(pct).toBeLessThan(100)
  })

  it('returns null for due date before domain', () => {
    const t = makeTask({ dueDate: '2025-12-01T00:00:00Z' })
    expect(dueDateMarkerPosition(t, domain)).toBeNull()
  })

  it('returns null for due date after domain', () => {
    const t = makeTask({ dueDate: '2026-03-01T00:00:00Z' })
    expect(dueDateMarkerPosition(t, domain)).toBeNull()
  })

  it('returns null for zero-width domain', () => {
    const d = { min: new Date('2026-01-01'), max: new Date('2026-01-01') }
    const t = makeTask({ dueDate: '2026-01-01T00:00:00Z' })
    expect(dueDateMarkerPosition(t, d)).toBeNull()
  })
})
