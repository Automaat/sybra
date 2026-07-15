import { describe, it, expect } from 'vitest'
import { formatElapsed } from './elapsed.js'

const START = '2026-04-01T00:00:00Z'
const BASE_MS = new Date(START).getTime()

describe('formatElapsed', () => {
  it.each([
    [0, '0s'],
    [1, '1s'],
    [59, '59s'],
    [60, '1m 0s'],
    [90, '1m 30s'],
    [119, '1m 59s'],
    [3600, '1h 00m'],
    [3661, '1h 01m'],
    [7322, '2h 02m'],
  ])('formats %ds correctly as %s', (seconds, expected) => {
    expect(formatElapsed(START, BASE_MS + seconds * 1000)).toBe(expected)
  })

  it('returns empty string for empty input', () => {
    expect(formatElapsed('', BASE_MS)).toBe('')
  })

  it('returns empty string for invalid date', () => {
    expect(formatElapsed('not-a-date', BASE_MS)).toBe('')
  })

  it('returns "0s" for negative diff (future start)', () => {
    expect(formatElapsed(START, BASE_MS - 5000)).toBe('0s')
  })
})
