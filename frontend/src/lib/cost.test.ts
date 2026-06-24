import { describe, it, expect } from 'vitest'
import { formatCost } from './cost.js'

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
