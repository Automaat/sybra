// Shared "needs you" predicate — the single source of truth for the red
// attention accent on TaskCard, the board toolbar counter (TaskList), and the
// global SideRail/BottomTabBar badge. Mirrors the three signals that already
// drove TaskList's inline predicate: awaits-human status, own-PR phases that
// require a concrete action, and inbound-review phases that require one.

import { awaitsHuman } from './statuses.js'
import { prPhaseNeedsYou } from './pr-phase.js'
import { reviewPhaseNeedsYou } from './review-phase.js'

export interface AttentionTask {
  status: string
  tags?: string[]
  prPhase?: string
  reviewPhase?: string
}

/** True when a task currently awaits a concrete user action. */
export function taskNeedsUserAttention(t: AttentionTask): boolean {
  return awaitsHuman(t.status) || prPhaseNeedsYou(t) || reviewPhaseNeedsYou(t)
}

/**
 * Same signal, restricted to active (non-terminal) tasks — for global counts
 * (SideRail/BottomTabBar badge) where a `done`/`cancelled` task must never
 * contribute even if its stale status/phase fields would otherwise match.
 */
export function activeTaskNeedsUserAttention(t: AttentionTask): boolean {
  if (t.status === 'done' || t.status === 'cancelled') return false
  return taskNeedsUserAttention(t)
}
