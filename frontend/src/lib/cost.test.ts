import { describe, it, expect } from 'vitest'
import { formatCost, formatCostShort } from './cost.js'

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
