import { describe, it, expect } from 'vitest'
import { formatCost, formatCostShort, taskTotalCost, taskRunCount } from './cost.js'

function runs(costs: (number | null | undefined)[]) {
  return { agentRuns: costs.map((c) => ({ costUsd: c })) } as any
}

describe('taskTotalCost', () => {
  it('sums 0 runs to 0', () => {
    expect(taskTotalCost(runs([]))).toBe(0)
  })

  it('sums a single run', () => {
    expect(taskTotalCost(runs([1.5]))).toBe(1.5)
  })

  it('sums mixed valid costs', () => {
    expect(taskTotalCost(runs([1, 2.5, 0.25]))).toBe(3.75)
  })

  it('drops null costUsd from the total', () => {
    expect(taskTotalCost(runs([1, null]))).toBe(1)
  })

  it('drops undefined costUsd from the total', () => {
    expect(taskTotalCost(runs([1, undefined]))).toBe(1)
  })

  it('drops NaN costUsd from the total', () => {
    expect(taskTotalCost(runs([1, Number.NaN]))).toBe(1)
  })

  it('drops negative costUsd from the total', () => {
    expect(taskTotalCost(runs([1, -5]))).toBe(1)
  })

  it('drops ±Infinity costUsd from the total', () => {
    expect(taskTotalCost(runs([1, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]))).toBe(1)
  })

  it('handles missing agentRuns entirely', () => {
    expect(taskTotalCost({} as any)).toBe(0)
  })
})

describe('taskRunCount', () => {
  it('counts 0 runs', () => {
    expect(taskRunCount(runs([]))).toBe(0)
  })

  it('counts 1 run', () => {
    expect(taskRunCount(runs([1]))).toBe(1)
  })

  it('counts every run regardless of cost validity', () => {
    expect(taskRunCount(runs([1, null, undefined, Number.NaN, -5]))).toBe(5)
  })

  it('handles missing agentRuns entirely', () => {
    expect(taskRunCount({} as any)).toBe(0)
  })
})

describe('formatCost', () => {
  it('uses a uniform 4-decimal precision', () => {
    expect(formatCost(0.2758)).toBe('$0.2758')
    expect(formatCost(0.28)).toBe('$0.2800')
  })

  it('keeps sub-cent costs visible', () => {
    expect(formatCost(0.0028)).toBe('$0.0028')
  })

  it('handles zero and whole values', () => {
    expect(formatCost(0)).toBe('$0.0000')
    expect(formatCost(5)).toBe('$5.0000')
  })

  it('coerces non-finite input to zero instead of $NaN', () => {
    expect(formatCost(undefined)).toBe('$0.0000')
    expect(formatCost(null)).toBe('$0.0000')
    expect(formatCost(Number.NaN)).toBe('$0.0000')
  })
})

describe('formatCostShort', () => {
  it('uses two decimals for at-a-glance lists', () => {
    expect(formatCostShort(0.88)).toBe('$0.88')
    expect(formatCostShort(0.8758)).toBe('$0.88')
    expect(formatCostShort(5)).toBe('$5.00')
  })

  it('floors real sub-cent costs to <$0.01 instead of $0.00', () => {
    expect(formatCostShort(0.0028)).toBe('<$0.01')
  })

  it('shows exact zero as $0.00', () => {
    expect(formatCostShort(0)).toBe('$0.00')
  })

  it('coerces non-finite input to zero', () => {
    expect(formatCostShort(undefined)).toBe('$0.00')
    expect(formatCostShort(Number.NaN)).toBe('$0.00')
  })
})
