import { describe, it, expect } from 'vitest'
import {
  CORE_STATUSES,
  CORE_STATUS_OPTIONS,
  coreStatus,
  statusOptionsFor,
  BOARD_COLUMNS,
  STATUS_MAP,
} from './statuses.js'

describe('CORE_STATUSES', () => {
  it('is the active board columns plus the terminal states', () => {
    expect(CORE_STATUSES).toEqual([...BOARD_COLUMNS.map((c) => c.status), 'done', 'cancelled'])
  })

  it('is a small set (<= 8) and excludes granular states', () => {
    expect(CORE_STATUSES.length).toBeLessThanOrEqual(8)
    for (const granular of ['new', 'plan-review', 'ready-review', 'test-plan-review', 'blocked']) {
      expect(CORE_STATUSES).not.toContain(granular)
    }
  })

  it('exposes options with labels from STATUS_MAP', () => {
    for (const opt of CORE_STATUS_OPTIONS) {
      expect(opt.label).toBe(STATUS_MAP[opt.value].label)
    }
  })
})

describe('coreStatus', () => {
  it('rolls granular states up to their board column', () => {
    expect(coreStatus('new')).toBe('todo')
    expect(coreStatus('plan-review')).toBe('planning')
    expect(coreStatus('ready-review')).toBe('in-review')
    expect(coreStatus('test-plan-review')).toBe('testing')
    expect(coreStatus('blocked')).toBe('human-required')
  })

  it('maps a column status to itself', () => {
    for (const col of BOARD_COLUMNS) {
      expect(coreStatus(col.status)).toBe(col.status)
    }
  })

  it('passes terminal/unknown states through unchanged', () => {
    expect(coreStatus('done')).toBe('done')
    expect(coreStatus('cancelled')).toBe('cancelled')
    expect(coreStatus('mystery')).toBe('mystery')
  })

  it('always lands on a core status for every folded state', () => {
    for (const col of BOARD_COLUMNS) {
      for (const folded of col.includes) {
        expect(CORE_STATUSES).toContain(coreStatus(folded))
      }
    }
  })
})

describe('statusOptionsFor', () => {
  it('returns just the core options for a core status', () => {
    expect(statusOptionsFor('in-progress')).toBe(CORE_STATUS_OPTIONS)
  })

  it('returns just the core options for a folded or terminal status', () => {
    expect(statusOptionsFor('blocked')).toBe(CORE_STATUS_OPTIONS)
    expect(statusOptionsFor('cancelled')).toBe(CORE_STATUS_OPTIONS)
  })

  it('appends an unknown current status so it stays selectable', () => {
    const opts = statusOptionsFor('mystery')
    expect(opts).toHaveLength(CORE_STATUS_OPTIONS.length + 1)
    expect(opts.at(-1)).toEqual({ value: 'mystery', label: 'mystery' })
  })
})
