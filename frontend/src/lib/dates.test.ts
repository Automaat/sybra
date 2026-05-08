import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import {
  parseNaturalDate,
  formatDueDateDisplay,
  formatDate,
  toUtcDayStart,
  toUtcDayEnd,
} from './dates.js'

describe('parseNaturalDate', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-05-15T10:00:00Z'))
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('returns null for empty / clear / none', () => {
    expect(parseNaturalDate('')).toBeNull()
    expect(parseNaturalDate('  ')).toBeNull()
    expect(parseNaturalDate('none')).toBeNull()
    expect(parseNaturalDate('clear')).toBeNull()
  })
  it('parses today/tomorrow/yesterday', () => {
    const today = parseNaturalDate('today')!
    expect(today.getDate()).toBe(new Date().getDate())
    const tomorrow = parseNaturalDate('tomorrow')!
    expect(tomorrow.getTime() - today.getTime()).toBe(86400000)
  })
  it('parses next monday relative to system time', () => {
    const d = parseNaturalDate('next monday')!
    expect(d.getDay()).toBe(1)
    expect(d.getTime()).toBeGreaterThan(Date.now())
  })
  it('parses "in N days/weeks"', () => {
    function expectedDate(daysFromNow: number): { date: number; month: number } {
      const d = new Date()
      d.setDate(d.getDate() + daysFromNow)
      return { date: d.getDate(), month: d.getMonth() }
    }
    const inThree = parseNaturalDate('in 3 days')!
    const exp3 = expectedDate(3)
    expect(inThree.getDate()).toBe(exp3.date)
    expect(inThree.getMonth()).toBe(exp3.month)
    const inTwoWeeks = parseNaturalDate('in 2 weeks')!
    const exp14 = expectedDate(14)
    expect(inTwoWeeks.getDate()).toBe(exp14.date)
    expect(inTwoWeeks.getMonth()).toBe(exp14.month)
  })
  it('parses ISO date', () => {
    const d = parseNaturalDate('2026-12-31')!
    expect(d.getUTCFullYear()).toBe(2026)
    expect(d.getUTCMonth()).toBe(11)
    expect(d.getUTCDate()).toBe(31)
  })
  it('returns null on unparseable input', () => {
    expect(parseNaturalDate('purple')).toBeNull()
    expect(parseNaturalDate('not a date at all')).toBeNull()
  })
})

describe('formatDueDateDisplay', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-05-15T10:00:00Z'))
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('returns "Set due date" for null/undefined/invalid', () => {
    expect(formatDueDateDisplay(null)).toBe('Set due date')
    expect(formatDueDateDisplay(undefined)).toBe('Set due date')
    expect(formatDueDateDisplay('not-a-date')).toBe('Set due date')
  })
  it('returns Today/Tomorrow/Yesterday', () => {
    expect(formatDueDateDisplay('2026-05-15T15:00:00Z')).toBe('Today')
    expect(formatDueDateDisplay('2026-05-16T15:00:00Z')).toBe('Tomorrow')
    expect(formatDueDateDisplay('2026-05-14T15:00:00Z')).toBe('Yesterday')
  })
  it('returns "In N days" for 2..6 days out', () => {
    expect(formatDueDateDisplay('2026-05-18T15:00:00Z')).toBe('In 3 days')
  })
  it('returns "Nd overdue" for past dates', () => {
    expect(formatDueDateDisplay('2026-05-10T15:00:00Z')).toBe('5d overdue')
  })
})

describe('formatDate', () => {
  it('returns "-" for falsy', () => {
    expect(formatDate(null)).toBe('-')
    expect(formatDate(undefined)).toBe('-')
  })
  it('returns localized string for valid date', () => {
    expect(formatDate('2026-01-01T00:00:00Z')).not.toBe('-')
  })
})

describe('toUtcDayStart / toUtcDayEnd', () => {
  it('returns null for empty', () => {
    expect(toUtcDayStart('')).toBeNull()
    expect(toUtcDayEnd('')).toBeNull()
  })
  it('returns midnight UTC for start', () => {
    const d = toUtcDayStart('2026-04-15')!
    expect(d.toISOString()).toBe('2026-04-15T00:00:00.000Z')
  })
  it('returns 23:59:59.999 UTC for end', () => {
    const d = toUtcDayEnd('2026-04-15')!
    expect(d.toISOString()).toBe('2026-04-15T23:59:59.999Z')
  })
})
