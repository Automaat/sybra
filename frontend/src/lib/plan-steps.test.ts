import { describe, it, expect } from 'vitest'
import { extractLatestPlanSteps } from './plan-steps.js'
import type { TimestampedStreamEvent } from './timeline.js'

function makeStreamEvent(overrides: Record<string, unknown> = {}): TimestampedStreamEvent {
  return {
    event: {
      type: 'assistant',
      content: '',
      plan_steps: undefined,
      ...overrides,
    } as any,
    receivedAt: new Date(),
  }
}

describe('extractLatestPlanSteps', () => {
  it('returns empty array for no events', () => {
    expect(extractLatestPlanSteps([])).toEqual([])
  })

  it('returns empty array when no events have plan_steps', () => {
    const events = [
      makeStreamEvent({ plan_steps: undefined }),
      makeStreamEvent({ plan_steps: [] }),
    ]
    expect(extractLatestPlanSteps(events)).toEqual([])
  })

  it('returns steps from the last event with non-empty plan_steps', () => {
    const steps = [{ content: 'Do the thing', status: 'pending' }]
    const events = [
      makeStreamEvent({ plan_steps: steps }),
      makeStreamEvent({ plan_steps: undefined }),
    ]
    expect(extractLatestPlanSteps(events)).toEqual(steps)
  })

  it('returns steps from the most recent event with non-empty plan_steps', () => {
    const old = [{ content: 'Old step', status: 'completed' }]
    const latest = [{ content: 'New step', status: 'in_progress' }]
    const events = [
      makeStreamEvent({ plan_steps: old }),
      makeStreamEvent({ plan_steps: latest }),
      makeStreamEvent({ plan_steps: undefined }),
    ]
    expect(extractLatestPlanSteps(events)).toEqual(latest)
  })

  it('skips events with empty plan_steps arrays', () => {
    const steps = [{ content: 'Real step', status: 'pending' }]
    const events = [
      makeStreamEvent({ plan_steps: steps }),
      makeStreamEvent({ plan_steps: [] }),
    ]
    expect(extractLatestPlanSteps(events)).toEqual(steps)
  })
})
