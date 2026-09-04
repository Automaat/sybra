import type { TimestampedStreamEvent } from './timeline.js'

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
