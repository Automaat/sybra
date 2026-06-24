import { describe, it, expect } from 'vitest'
import { statusSummary } from './status-summary.js'
import { statusLabel } from './statuses.js'

describe('statusSummary', () => {
  it('surfaces awaiting-human states as attention with an action hint', () => {
    const s = statusSummary('plan-review')
    expect(s).toEqual({ label: 'Plan Review', hint: 'awaiting your approval', tone: 'attention' })
  })

  it('uses the canonical label, never an invented one', () => {
    expect(statusSummary('human-required')?.label).toBe(statusLabel('human-required'))
    expect(statusSummary('blocked')?.label).toBe(statusLabel('blocked'))
  })

  it('surfaces a non-attention folded sub-state quietly as info', () => {
    // ready-review rolls up to the in-review column; new rolls up to todo.
    expect(statusSummary('ready-review')).toEqual({ label: statusLabel('ready-review'), hint: '', tone: 'info' })
    expect(statusSummary('new')).toEqual({ label: statusLabel('new'), hint: '', tone: 'info' })
  })

  it('returns null for a plain core status that matches its column', () => {
    expect(statusSummary('in-progress')).toBeNull()
    expect(statusSummary('todo')).toBeNull()
    expect(statusSummary('testing')).toBeNull()
  })

  it('returns null for terminal states', () => {
    expect(statusSummary('done')).toBeNull()
    expect(statusSummary('cancelled')).toBeNull()
  })
})
