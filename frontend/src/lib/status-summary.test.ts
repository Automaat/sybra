import { describe, it, expect } from 'vitest'
import { statusSummary } from './status-summary.js'
import { statusLabel } from './statuses.js'

describe('statusSummary', () => {
  it('surfaces awaiting-human states as attention with an action hint', () => {
    expect(statusSummary('plan-review')).toEqual({
      label: 'Plan Review',
      hint: 'awaiting your approval',
      tone: 'attention',
    })
    expect(statusSummary('human-required')?.tone).toBe('attention')
    expect(statusSummary('blocked')?.tone).toBe('attention')
  })

  it('summarises agent/pipeline states as info', () => {
    for (const s of ['planning', 'in-progress', 'in-review', 'testing', 'ready-review', 'new']) {
      expect(statusSummary(s)?.tone).toBe('info')
      expect(statusSummary(s)?.hint).toBeTruthy()
    }
  })

  it('uses the canonical label, never an invented one', () => {
    for (const s of ['plan-review', 'human-required', 'in-progress', 'ready-review']) {
      expect(statusSummary(s)?.label).toBe(statusLabel(s))
    }
  })

  it('returns null for quiet/terminal/unknown states (no banner)', () => {
    expect(statusSummary('todo')).toBeNull()
    expect(statusSummary('done')).toBeNull()
    expect(statusSummary('cancelled')).toBeNull()
    expect(statusSummary('mystery')).toBeNull()
  })
})
