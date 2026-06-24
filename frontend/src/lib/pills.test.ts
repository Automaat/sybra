import { describe, it, expect } from 'vitest'
import { PILL_ROLE_CLASS, pillClass, type PillRole } from './pills.js'

const ROLES: PillRole[] = ['status', 'attention', 'tag', 'reference', 'project']

describe('pill roles', () => {
  it('defines a treatment for every role', () => {
    for (const role of ROLES) {
      expect(PILL_ROLE_CLASS[role]).toBeTruthy()
    }
  })

  it('gives each role a distinct treatment', () => {
    const treatments = ROLES.map((r) => PILL_ROLE_CLASS[r])
    expect(new Set(treatments).size).toBe(ROLES.length)
  })

  // A bg-/text-/border- utility keyed to any brand or status palette.
  const PALETTE_COLOUR = /(?:bg|text|border)-(?:primary|secondary|tertiary|warning|error|success)/

  it('keeps the passive roles monochrome (no status/brand colour baked in)', () => {
    for (const role of ['tag', 'reference', 'project'] as PillRole[]) {
      expect(PILL_ROLE_CLASS[role]).not.toMatch(PALETTE_COLOUR)
    }
  })

  it('lets status/attention stay colourless so the caller layers status colour', () => {
    for (const role of ['status', 'attention'] as PillRole[]) {
      expect(PILL_ROLE_CLASS[role]).not.toMatch(PALETTE_COLOUR)
      expect(PILL_ROLE_CLASS[role]).not.toMatch(/\bbg-\S/)
    }
  })
})

describe('pillClass', () => {
  it('returns the role treatment unchanged with no extra', () => {
    expect(pillClass('tag')).toBe(PILL_ROLE_CLASS.tag)
  })

  it('appends extra classes (e.g. status colour) after the role treatment', () => {
    expect(pillClass('status', 'bg-error-200 text-error-800')).toBe(
      `${PILL_ROLE_CLASS.status} bg-error-200 text-error-800`,
    )
  })
})
