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
  | 'conflict'

/** Icon keys, resolved to lucide components at the call site. */
export type ReviewPhaseIcon = 'loader' | 'eye' | 'pen' | 'hourglass' | 'shield' | 'check' | 'conflict'

export interface ReviewPhaseMeta {
  label: string
  icon: ReviewPhaseIcon
  /** bg + text pill classes (light + dark) */
  classes: string
}

// Needs-you phases drive the red tile accent via reviewPhaseNeedsYou. Drafted
// and manual also use human-required status, but needs-approval intentionally
// stays in-review so it cannot start another automatic diagnostic review.
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
  // Blocked on the author rebasing — error-toned so it reads as a problem, but
  // it sorts to the bottom (see REVIEW_PHASE_ORDER): nothing for you to do yet.
  conflict: {
    label: 'Conflict',
    icon: 'conflict',
    classes: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200',
  },
}

// Lane sort: the user's pending actions first, then waiting, then done, and
// conflicting PRs last — they're blocked on the author, so they sink to the
// bottom of the lane.
const REVIEW_PHASE_ORDER: ReviewPhase[] = [
  'needs-approval',
  'drafted',
  'manual',
  'reviewing',
  'awaiting-author',
  'approved',
  'conflict',
]

/** True when a task is an inbound PR review (reviewing someone else's code). */
export function isReviewTask(t: { tags?: string[] }): boolean {
  return t.tags?.includes('review') ?? false
}

/** Legacy tag once used for self-authored PR handoffs. New handoffs do not set it. */
export const HANDOFF_PR_TAG = 'handoff-pr'

/**
 * True when a review task is a self-authored PR handed off for cross-provider
 * review. It keeps the `review` tag (so it reuses the pr-review workflow and the
 * review-phase chrome), but it is the user's *own* PR — not inbound — so it must
 * render in the In Review status column, never the inbound "To Review" lane.
 */
export function isHandoffPRReview(t: { tags?: string[] }): boolean {
  return t.tags?.includes(HANDOFF_PR_TAG) ?? false
}

/**
 * True only for INBOUND PR reviews (reviewing someone else's code) — the set
 * that lives in the tag-based "To Review" board lane. Self-authored handoff PR
 * reviews are excluded: they belong in their own status column. This is the
 * single predicate that splits the To Review lane from the status columns.
 */
export function isInboundReview(t: { tags?: string[] }): boolean {
  return isReviewTask(t) && !isHandoffPRReview(t)
}

/** A task's review phase, defaulting unknown/empty values to `reviewing`. */
export function reviewPhaseOf(t: { reviewPhase?: string }): ReviewPhase {
  const p = t.reviewPhase
  // Own-property check: `in` would also match inherited keys like `toString`,
  // letting corrupt data slip through and crash reviewPhaseMeta().
  return p && Object.hasOwn(REVIEW_PHASE_META, p) ? (p as ReviewPhase) : 'reviewing'
}

export function reviewPhaseMeta(t: { reviewPhase?: string }): ReviewPhaseMeta {
  return REVIEW_PHASE_META[reviewPhaseOf(t)]
}

/** Sort key for the PR Reviews lane — lower sorts first (needs-you on top). */
export function reviewPhaseRank(t: { reviewPhase?: string }): number {
  const idx = REVIEW_PHASE_ORDER.indexOf(reviewPhaseOf(t))
  return idx === -1 ? REVIEW_PHASE_ORDER.length : idx
}

const REVIEW_PHASE_NEEDS_YOU: ReadonlySet<ReviewPhase> = new Set<ReviewPhase>([
  'manual',
  'drafted',
  'needs-approval',
])

/** True when an inbound PR review task awaits the user's review action. */
export function reviewPhaseNeedsYou(t: { tags?: string[]; reviewPhase?: string }): boolean {
  return isReviewTask(t) && REVIEW_PHASE_NEEDS_YOU.has(reviewPhaseOf(t))
}
