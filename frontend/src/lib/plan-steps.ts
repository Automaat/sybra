import type { TimestampedStreamEvent } from './timeline.js'
import type { agent } from '../../wailsjs/go/models.js'

export interface PlanStep {
  content: string
  status: string // "pending" | "in_progress" | "completed"
}

/**
 * Walk stream events in reverse to find the most recent non-empty TodoWrite
 * snapshot (carried on StreamEvent.plan_steps).
 */
export function extractLatestPlanSteps(events: TimestampedStreamEvent[]): PlanStep[] {
  for (let i = events.length - 1; i >= 0; i--) {
    const steps = events[i].event.plan_steps
    if (steps && steps.length > 0) {
      return steps
    }
  }
  return []
}

/**
 * Walk ConvoEvents in reverse to find the most recent TodoWrite tool use and
 * parse its todos array.
 */
export function extractLatestPlanStepsFromConvo(events: agent.ConvoEvent[]): PlanStep[] {
  for (let i = events.length - 1; i >= 0; i--) {
    const ev = events[i]
    if (!ev.toolUses) continue
    for (let j = ev.toolUses.length - 1; j >= 0; j--) {
      const tu = ev.toolUses[j]
      if (tu.name !== 'TodoWrite') continue
      const todos = tu.input?.todos
      if (!Array.isArray(todos) || todos.length === 0) continue
      const steps: PlanStep[] = []
      for (const item of todos) {
        if (typeof item?.content === 'string' && item.content) {
          steps.push({ content: item.content, status: item.status ?? 'pending' })
        }
      }
      if (steps.length > 0) return steps
    }
  }
  return []
}
