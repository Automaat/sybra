import { describe, it, expect } from 'vitest'
import {
  REVIEW_PHASE_META,
  isReviewTask,
  isHandoffPRReview,
  isInboundReview,
  reviewPhaseOf,
  reviewPhaseMeta,
  reviewPhaseNeedsYou,
  reviewPhaseRank,
  type ReviewPhase,
} from './review-phase.js'
import { BOARD_COLUMNS, BOARD_LANES, REVIEW_LANE, CORE_STATUSES } from './statuses.js'

describe('isReviewTask', () => {
  it('is true only when the review tag is present', () => {
    expect(isReviewTask({ tags: ['review'] })).toBe(true)
    expect(isReviewTask({ tags: ['backend', 'review'] })).toBe(true)
    expect(isReviewTask({ tags: ['backend'] })).toBe(false)
    expect(isReviewTask({})).toBe(false)
  })
})

describe('isHandoffPRReview', () => {
  it('is true only when the handoff-pr tag is present', () => {
    expect(isHandoffPRReview({ tags: ['review', 'handoff-pr'] })).toBe(true)
    expect(isHandoffPRReview({ tags: ['handoff-pr'] })).toBe(true)
    expect(isHandoffPRReview({ tags: ['review'] })).toBe(false)
    expect(isHandoffPRReview({})).toBe(false)
  })
})

describe('isInboundReview', () => {
  it('is true for review tasks that are not self-authored handoffs', () => {
    expect(isInboundReview({ tags: ['review'] })).toBe(true)
    expect(isInboundReview({ tags: ['backend', 'review'] })).toBe(true)
  })

  it('excludes self-authored handoff PR reviews (they belong in In Review)', () => {
    expect(isInboundReview({ tags: ['review', 'handoff-pr'] })).toBe(false)
  })

  it('is false for non-review tasks', () => {
    expect(isInboundReview({ tags: ['backend'] })).toBe(false)
    expect(isInboundReview({ tags: ['handoff-pr'] })).toBe(false)
    expect(isInboundReview({})).toBe(false)
  })
})

describe('reviewPhaseOf', () => {
  it('returns the known phase', () => {
    expect(reviewPhaseOf({ reviewPhase: 'drafted' })).toBe('drafted')
    expect(reviewPhaseOf({ reviewPhase: 'needs-approval' })).toBe('needs-approval')
  })

  it('falls back to reviewing for empty or unknown values', () => {
    expect(reviewPhaseOf({})).toBe('reviewing')
    expect(reviewPhaseOf({ reviewPhase: '' })).toBe('reviewing')
    expect(reviewPhaseOf({ reviewPhase: 'bogus' })).toBe('reviewing')
  })

  it('does not treat inherited Object keys as valid phases', () => {
    for (const proto of ['toString', 'constructor', 'hasOwnProperty', '__proto__']) {
      expect(reviewPhaseOf({ reviewPhase: proto })).toBe('reviewing')
    }
  })
})

describe('reviewPhaseMeta', () => {
  it('maps each phase to a non-empty label, icon, and classes', () => {
    for (const phase of Object.keys(REVIEW_PHASE_META) as ReviewPhase[]) {
      const m = REVIEW_PHASE_META[phase]
      expect(m.label).not.toBe('')
      expect(m.icon).not.toBe('')
      expect(m.classes).toContain('bg-')
    }
  })

  it('resolves a phase to its metadata', () => {
    expect(reviewPhaseMeta({ reviewPhase: 'drafted' }).label).toBe('Post review')
    expect(reviewPhaseMeta({ reviewPhase: 'needs-approval' }).label).toBe('Approve')
    expect(reviewPhaseMeta({}).label).toBe('Reviewing')
  })
})

describe('reviewPhaseRank', () => {
  it('sorts user-action phases ahead of waiting and approved', () => {
    const rank = (p: string) => reviewPhaseRank({ reviewPhase: p })
    expect(rank('needs-approval')).toBeLessThan(rank('awaiting-author'))
    expect(rank('drafted')).toBeLessThan(rank('awaiting-author'))
    expect(rank('awaiting-author')).toBeLessThan(rank('approved'))
  })

  it('sinks conflicting PRs to the bottom of the lane', () => {
    const rank = (p: string) => reviewPhaseRank({ reviewPhase: p })
    expect(rank('conflict')).toBeGreaterThan(rank('approved'))
    expect(rank('conflict')).toBeGreaterThan(rank('needs-approval'))
  })
})

describe('reviewPhaseNeedsYou', () => {
  it('flags inbound review phases that need explicit reviewer action', () => {
    expect(reviewPhaseNeedsYou({ tags: ['review'], reviewPhase: 'manual' })).toBe(true)
    expect(reviewPhaseNeedsYou({ tags: ['review'], reviewPhase: 'drafted' })).toBe(true)
    expect(reviewPhaseNeedsYou({ tags: ['review'], reviewPhase: 'needs-approval' })).toBe(true)
  })

  it('does not flag passive review phases or non-review tasks', () => {
    expect(reviewPhaseNeedsYou({ tags: ['review'], reviewPhase: 'awaiting-author' })).toBe(false)
    expect(reviewPhaseNeedsYou({ tags: ['review'], reviewPhase: 'approved' })).toBe(false)
    expect(reviewPhaseNeedsYou({ reviewPhase: 'needs-approval' })).toBe(false)
  })
})

describe('BOARD_LANES', () => {
  it('inserts the To Review lane immediately before Human Required', () => {
    const idx = BOARD_LANES.indexOf(REVIEW_LANE)
    expect(idx).toBeGreaterThan(0)
    expect(BOARD_LANES[idx + 1].status).toBe('human-required')
  })

  it('keeps every status column from BOARD_COLUMNS plus the one lane', () => {
    expect(BOARD_LANES.length).toBe(BOARD_COLUMNS.length + 1)
    for (const c of BOARD_COLUMNS) expect(BOARD_LANES).toContain(c)
  })

  it('keeps the review sentinel out of the pickable status set', () => {
    expect(CORE_STATUSES).not.toContain('reviews')
    expect(REVIEW_LANE.kind).toBe('review')
  })
})
