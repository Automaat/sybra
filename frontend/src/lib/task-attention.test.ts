import { describe, it, expect } from 'vitest'
import { taskNeedsUserAttention, activeTaskNeedsUserAttention } from './task-attention.js'

describe('taskNeedsUserAttention', () => {
  it.each(['human-required', 'blocked', 'plan-review'])('is true for awaits-human status %s', (status) => {
    expect(taskNeedsUserAttention({ status })).toBe(true)
  })

  it.each(['todo', 'in-progress', 'ready-review', 'testing', 'ready-pr'])(
    'is false for non-attention status %s with no PR/review phase',
    (status) => {
      expect(taskNeedsUserAttention({ status })).toBe(false)
    },
  )

  it.each(['draft', 'approved'])('is true for own-PR phase %s', (prPhase) => {
    expect(taskNeedsUserAttention({ status: 'in-review', prPhase })).toBe(true)
  })

  it.each(['building', 'fixing', 'changes-requested', 'awaiting-approval'])(
    'is false for own-PR phase %s',
    (prPhase) => {
      expect(taskNeedsUserAttention({ status: 'in-review', prPhase })).toBe(false)
    },
  )

  it.each(['manual', 'drafted', 'needs-approval'])('is true for inbound review phase %s', (reviewPhase) => {
    expect(taskNeedsUserAttention({ status: 'in-review', tags: ['review'], reviewPhase })).toBe(true)
  })

  it.each(['reviewing', 'awaiting-author', 'approved', 'conflict'])(
    'is false for inbound review phase %s',
    (reviewPhase) => {
      expect(taskNeedsUserAttention({ status: 'in-review', tags: ['review'], reviewPhase })).toBe(false)
    },
  )

  it('is false for a review phase on a task without the review tag', () => {
    expect(taskNeedsUserAttention({ status: 'in-review', reviewPhase: 'manual' })).toBe(false)
  })
})

describe('activeTaskNeedsUserAttention', () => {
  it('excludes done tasks even with an awaits-human status', () => {
    expect(activeTaskNeedsUserAttention({ status: 'done' })).toBe(false)
  })

  it('excludes cancelled tasks even with a needs-you PR phase', () => {
    expect(activeTaskNeedsUserAttention({ status: 'cancelled', prPhase: 'draft' })).toBe(false)
  })

  it('includes human-required tasks', () => {
    expect(activeTaskNeedsUserAttention({ status: 'human-required' })).toBe(true)
  })

  it('includes own-PR approved phase', () => {
    expect(activeTaskNeedsUserAttention({ status: 'in-review', prPhase: 'approved' })).toBe(true)
  })

  it('includes inbound review needs-approval phase', () => {
    expect(
      activeTaskNeedsUserAttention({ status: 'in-review', tags: ['review'], reviewPhase: 'needs-approval' }),
    ).toBe(true)
  })

  it('excludes active tasks that are not awaiting anything', () => {
    expect(activeTaskNeedsUserAttention({ status: 'in-progress' })).toBe(false)
  })
})
