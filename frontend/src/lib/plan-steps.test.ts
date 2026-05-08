import { describe, it, expect } from 'vitest'
import { extractLatestPlanSteps, extractLatestPlanStepsFromConvo } from './plan-steps.js'
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

function makeConvoEvent(overrides: Record<string, unknown> = {}) {
  return {
    type: 'assistant',
    text: '',
    toolUses: [],
    toolResults: [],
    timestamp: new Date().toISOString(),
    ...overrides,
  } as any
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

describe('extractLatestPlanStepsFromConvo', () => {
  it('returns empty array for no events', () => {
    expect(extractLatestPlanStepsFromConvo([])).toEqual([])
  })

  it('returns empty array when no toolUses exist', () => {
    const events = [makeConvoEvent({ toolUses: null })]
    expect(extractLatestPlanStepsFromConvo(events)).toEqual([])
  })

  it('returns empty array when no TodoWrite tool use found', () => {
    const events = [
      makeConvoEvent({
        toolUses: [{ name: 'Bash', input: {}, id: '1' }],
      }),
    ]
    expect(extractLatestPlanStepsFromConvo(events)).toEqual([])
  })

  it('extracts steps from TodoWrite tool use', () => {
    const events = [
      makeConvoEvent({
        toolUses: [
          {
            name: 'TodoWrite',
            id: 't1',
            input: {
              todos: [
                { content: 'First task', status: 'pending' },
                { content: 'Second task', status: 'in_progress' },
              ],
            },
          },
        ],
      }),
    ]
    const result = extractLatestPlanStepsFromConvo(events)
    expect(result).toHaveLength(2)
    expect(result[0].content).toBe('First task')
    expect(result[1].status).toBe('in_progress')
  })

  it('skips TodoWrite with empty todos', () => {
    const events = [
      makeConvoEvent({
        toolUses: [{ name: 'TodoWrite', id: 't1', input: { todos: [] } }],
      }),
    ]
    expect(extractLatestPlanStepsFromConvo(events)).toEqual([])
  })

  it('returns most recent TodoWrite result', () => {
    const events = [
      makeConvoEvent({
        toolUses: [{ name: 'TodoWrite', id: 't1', input: { todos: [{ content: 'Old', status: 'completed' }] } }],
      }),
      makeConvoEvent({
        toolUses: [{ name: 'TodoWrite', id: 't2', input: { todos: [{ content: 'New', status: 'pending' }] } }],
      }),
    ]
    const result = extractLatestPlanStepsFromConvo(events)
    expect(result[0].content).toBe('New')
  })

  it('defaults status to "pending" when not provided', () => {
    const events = [
      makeConvoEvent({
        toolUses: [
          { name: 'TodoWrite', id: 't1', input: { todos: [{ content: 'Task with no status' }] } },
        ],
      }),
    ]
    const result = extractLatestPlanStepsFromConvo(events)
    expect(result[0].status).toBe('pending')
  })

  it('skips todos without content', () => {
    const events = [
      makeConvoEvent({
        toolUses: [
          {
            name: 'TodoWrite',
            id: 't1',
            input: {
              todos: [
                { content: '', status: 'pending' },
                { content: 'Valid task', status: 'pending' },
              ],
            },
          },
        ],
      }),
    ]
    const result = extractLatestPlanStepsFromConvo(events)
    expect(result).toHaveLength(1)
    expect(result[0].content).toBe('Valid task')
  })
})
