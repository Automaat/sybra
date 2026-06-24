import { statusLabel } from './statuses.js'

// The standard "what do I do next?" slot on the detail page. It answers, in one
// line, what state the task is in and who it's waiting on — so the detail page
// never shows less state than the board (a plan-review task reads "Planning" in
// the core-status dropdown, but the banner says "Plan Review — awaiting your
// approval"). Terminal/quiet states (todo, done, cancelled) return null.

export type StatusTone = 'attention' | 'info'

export interface StatusSummary {
  /** Canonical status label — same vocabulary as board/list. */
  label: string
  /** A one-line "what's happening / what to do" hint. */
  hint: string
  tone: StatusTone
}

// `attention` = waiting on the user; `info` = an agent or the pipeline is
// working. Statuses absent here (todo, done, cancelled) get no banner.
const SUMMARY: Record<string, { hint: string; tone: StatusTone }> = {
  new: { hint: 'not yet triaged', tone: 'info' },
  planning: { hint: 'an agent is drafting a plan', tone: 'info' },
  'plan-review': { hint: 'awaiting your approval', tone: 'attention' },
  'in-progress': { hint: 'an agent is working on this', tone: 'info' },
  'ready-review': { hint: 'ready for review', tone: 'info' },
  'in-review': { hint: 'under review', tone: 'info' },
  testing: { hint: 'in testing', tone: 'info' },
  'test-plan-review': { hint: 'awaiting your approval', tone: 'attention' },
  'human-required': { hint: 'needs your input', tone: 'attention' },
  blocked: { hint: 'needs your response', tone: 'attention' },
}

/** The per-state summary to surface in the detail banner, or null when there's
 *  nothing useful to say (todo, terminal, or unknown statuses). */
export function statusSummary(status: string): StatusSummary | null {
  const meta = SUMMARY[status]
  if (!meta) return null
  return { label: statusLabel(status), hint: meta.hint, tone: meta.tone }
}
