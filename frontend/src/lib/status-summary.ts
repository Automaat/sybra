import { awaitsHuman, coreStatus, statusLabel } from './statuses.js'

// The detail page must never show *less* state than the board: a board card in
// the "Planning" column can read "Plan Review" (awaiting you), but the detail
// status control only shows the rolled-up column status. This derives the
// actionable sub-state to surface as a banner under the title.

export type StatusTone = 'attention' | 'info'

export interface StatusSummary {
  /** Canonical status label — same vocabulary as board/list. */
  label: string
  /** What the user should do, when the state implies an action. */
  hint: string
  tone: StatusTone
}

const HINTS: Record<string, string> = {
  'plan-review': 'awaiting your approval',
  'test-plan-review': 'awaiting your approval',
  'human-required': 'needs your input',
  blocked: 'needs your response',
}

/**
 * The sub-state worth surfacing on detail, or null for a plain core status that
 * matches its column (those get a per-state summary separately). Awaiting-human
 * states are `attention`; other granular states folded into a column (new,
 * ready-review) are surfaced quietly as `info` so detail still mirrors the board.
 */
export function statusSummary(status: string): StatusSummary | null {
  if (awaitsHuman(status)) {
    return { label: statusLabel(status), hint: HINTS[status] ?? '', tone: 'attention' }
  }
  if (coreStatus(status) !== status) {
    return { label: statusLabel(status), hint: '', tone: 'info' }
  }
  return null
}
