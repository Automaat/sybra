// Lifecycle phases for inbound PR-review tasks (tag `review`) — the board's
// "PR Reviews" lane colours each tile by phase. Mirrors the Go constants in
// internal/sybra/review_phase.go. Hue→token mapping follows the amber theme
// convention documented in agent-phases.ts (warning = violet, secondary =
// teal/blue, primary = amber, success = green).

export type ReviewPhase =
  | 'reviewing'
  | 'manual'
  | 'drafted'
  | 'awaiting-author'
  | 'needs-approval'
  | 'approved'

/** Icon keys, resolved to lucide components at the call site. */
export type ReviewPhaseIcon = 'loader' | 'eye' | 'pen' | 'hourglass' | 'shield' | 'check'

export interface ReviewPhaseMeta {
  label: string
  icon: ReviewPhaseIcon
  /** bg + text pill classes (light + dark) */
  classes: string
}

// The needs-you signal (red tile accent) is driven by the task's status, not
// duplicated here — the poller sets human-required for the manual/drafted/
// needs-approval phases, so awaitsHuman(status) stays the single source of
// truth and can't drift from this table.
export const REVIEW_PHASE_META: Record<ReviewPhase, ReviewPhaseMeta> = {
  reviewing: {
    label: 'Reviewing',
    icon: 'loader',
    classes: 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-300',
  },
  manual: {
    label: 'Your review',
    icon: 'eye',
    classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200',
  },
  drafted: {
    label: 'Post review',
    icon: 'pen',
    classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200',
  },
  'awaiting-author': {
    label: 'Awaiting author',
    icon: 'hourglass',
    classes: 'bg-secondary-200 text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200',
  },
  'needs-approval': {
    label: 'Approve',
    icon: 'shield',
    classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
  },
  approved: {
    label: 'Approved',
    icon: 'check',
    classes: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200',
  },
}

// Lane sort: the user's pending actions first, then waiting, then done.
const REVIEW_PHASE_ORDER: ReviewPhase[] = [
  'needs-approval',
  'drafted',
  'manual',
  'reviewing',
  'awaiting-author',
  'approved',
]

/** True when a task is an inbound PR review (reviewing someone else's code). */
export function isReviewTask(t: { tags?: string[] }): boolean {
  return t.tags?.includes('review') ?? false
}

/** A task's review phase, defaulting unknown/empty values to `reviewing`. */
export function reviewPhaseOf(t: { reviewPhase?: string }): ReviewPhase {
  const p = t.reviewPhase
  return p && p in REVIEW_PHASE_META ? (p as ReviewPhase) : 'reviewing'
}

export function reviewPhaseMeta(t: { reviewPhase?: string }): ReviewPhaseMeta {
  return REVIEW_PHASE_META[reviewPhaseOf(t)]
}

/** Sort key for the PR Reviews lane — lower sorts first (needs-you on top). */
export function reviewPhaseRank(t: { reviewPhase?: string }): number {
  const idx = REVIEW_PHASE_ORDER.indexOf(reviewPhaseOf(t))
  return idx === -1 ? REVIEW_PHASE_ORDER.length : idx
}
