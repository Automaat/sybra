import { describe, it, expect } from 'vitest'
import {
  PR_PHASE_META,
  isOwnPRTask,
  prPhaseOf,
  prPhaseMeta,
  prPhaseRank,
  prPhaseNeedsYou,
  type PRPhase,
} from './pr-phase.js'

describe('isOwnPRTask', () => {
  it('is true only for non-review tasks that carry a phase', () => {
    expect(isOwnPRTask({ prPhase: 'draft' })).toBe(true)
    expect(isOwnPRTask({ tags: ['backend'], prPhase: 'approved' })).toBe(true)
    expect(isOwnPRTask({ prPhase: '' })).toBe(false)
    expect(isOwnPRTask({})).toBe(false)
    // A review-tagged task is inbound — never an own-PR task, even with a phase.
    expect(isOwnPRTask({ tags: ['review'], prPhase: 'draft' })).toBe(false)
  })
})

describe('prPhaseOf', () => {
  it('returns the known phase', () => {
    expect(prPhaseOf({ prPhase: 'draft' })).toBe('draft')
    expect(prPhaseOf({ prPhase: 'awaiting-approval' })).toBe('awaiting-approval')
  })

  it('falls back to building for empty or unknown values', () => {
    expect(prPhaseOf({})).toBe('building')
    expect(prPhaseOf({ prPhase: '' })).toBe('building')
    expect(prPhaseOf({ prPhase: 'bogus' })).toBe('building')
  })

  it('does not treat inherited Object keys as valid phases', () => {
    for (const proto of ['toString', 'constructor', 'hasOwnProperty', '__proto__']) {
      expect(prPhaseOf({ prPhase: proto })).toBe('building')
    }
  })
})

describe('prPhaseMeta', () => {
  it('maps each phase to a non-empty label, icon, and classes', () => {
    for (const phase of Object.keys(PR_PHASE_META) as PRPhase[]) {
      const m = PR_PHASE_META[phase]
      expect(m.label).not.toBe('')
      expect(m.icon).not.toBe('')
      expect(m.classes).toContain('bg-')
    }
  })

  it('resolves a phase to its metadata', () => {
    expect(prPhaseMeta({ prPhase: 'draft' }).label).toBe('Draft — mark ready')
    expect(prPhaseMeta({ prPhase: 'approved' }).label).toBe('Approved — merge')
    expect(prPhaseMeta({}).label).toBe('Building')
  })
})

describe('prPhaseRank', () => {
  it('sorts user-action phases ahead of waiting and agent-owned ones', () => {
    const rank = (p: string) => prPhaseRank({ prPhase: p })
    expect(rank('approved')).toBeLessThan(rank('awaiting-approval'))
    expect(rank('awaiting-approval')).toBeLessThan(rank('changes-requested'))
    expect(rank('draft')).toBeLessThan(rank('building'))
    expect(rank('changes-requested')).toBeLessThan(rank('fixing'))
  })
})

describe('prPhaseNeedsYou', () => {
  it('flags only the strict your-court phases: draft and approved', () => {
    expect(prPhaseNeedsYou({ prPhase: 'draft' })).toBe(true)
    expect(prPhaseNeedsYou({ prPhase: 'approved' })).toBe(true)
  })

  it('does not flag waiting, agent-owned, or passive phases', () => {
    expect(prPhaseNeedsYou({ prPhase: 'awaiting-approval' })).toBe(false)
    expect(prPhaseNeedsYou({ prPhase: 'building' })).toBe(false)
    expect(prPhaseNeedsYou({ prPhase: 'fixing' })).toBe(false)
    expect(prPhaseNeedsYou({ prPhase: 'changes-requested' })).toBe(false)
  })

  it('never flags a task without a phase or a review task', () => {
    expect(prPhaseNeedsYou({})).toBe(false)
    expect(prPhaseNeedsYou({ tags: ['review'], prPhase: 'draft' })).toBe(false)
  })
})
