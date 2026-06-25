// Lifecycle phases for outbound own-PR tasks (status in-review/ready-review, not
// tag `review`) — the board's In Review cards show a colour-coded phase glyph.
// Mirrors the Go constants in internal/sybra/pr_phase.go. The phase is a pure
// overlay; it never changes the task's status (own-PR tasks stay in In Review).

import { isReviewTask } from './review-phase.js'

export type PRPhase =
  | 'draft'
  | 'building'
  | 'fixing'
  | 'changes-requested'
  | 'awaiting-approval'
  | 'approved'

/** Icon keys, resolved to lucide components at the call site. */
export type PRPhaseIcon = 'draft' | 'loader' | 'wrench' | 'comment' | 'hourglass' | 'check'

export interface PRPhaseMeta {
  label: string
  icon: PRPhaseIcon
  /** bg + text pill classes (light + dark) */
  classes: string
}

// Needs-you phases (draft, approved) drive the red tile accent via
// prPhaseNeedsYou — the colour here is the phase hue, distinct from that
// attention signal. `awaiting-approval` is a waiting state, not needs-you.
export const PR_PHASE_META: Record<PRPhase, PRPhaseMeta> = {
  draft: {
    label: 'Draft — mark ready',
    icon: 'draft',
    classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200',
  },
  building: {
    label: 'Building',
    icon: 'loader',
    classes: 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-300',
  },
  fixing: {
    label: 'Fixing',
    icon: 'wrench',
    classes: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200',
  },
  'changes-requested': {
    label: 'Comments',
    icon: 'comment',
    classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
  },
  'awaiting-approval': {
    label: 'Awaiting approval',
    icon: 'hourglass',
    classes: 'bg-secondary-200 text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200',
  },
  approved: {
    label: 'Approved — merge',
    icon: 'check',
    classes: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200',
  },
}

// Column sort: the user's pending actions first (merge, ping, mark-ready), then
// the auto-handled / waiting phases.
const PR_PHASE_ORDER: PRPhase[] = [
  'approved',
  'awaiting-approval',
  'draft',
  'changes-requested',
  'building',
  'fixing',
]

// Phases where the ball is strictly in the user's court (mark ready / merge) —
// drives the red card accent. `awaiting-approval` is deliberately excluded: the
// PR is waiting on reviewers, not on a concrete action the user must take, so
// it stays a visible-but-passive teal phase rather than adding red noise.
const PR_PHASE_NEEDS_YOU: ReadonlySet<PRPhase> = new Set<PRPhase>([
  'draft',
  'approved',
])

/** True when a task is one of the user's own PRs with a computed phase. */
export function isOwnPRTask(t: { tags?: string[]; prPhase?: string }): boolean {
  return !isReviewTask(t) && !!t.prPhase
}

/** A task's PR phase, defaulting unknown/empty values to `building`. */
export function prPhaseOf(t: { prPhase?: string }): PRPhase {
  const p = t.prPhase
  // Own-property check: `in` would also match inherited keys like `toString`,
  // letting corrupt data slip through and crash prPhaseMeta().
  return p && Object.hasOwn(PR_PHASE_META, p) ? (p as PRPhase) : 'building'
}

export function prPhaseMeta(t: { prPhase?: string }): PRPhaseMeta {
  return PR_PHASE_META[prPhaseOf(t)]
}

/** Sort key for the In Review column — lower sorts first (needs-you on top). */
export function prPhaseRank(t: { prPhase?: string }): number {
  const idx = PR_PHASE_ORDER.indexOf(prPhaseOf(t))
  return idx === -1 ? PR_PHASE_ORDER.length : idx
}

/**
 * True when an own-PR task's phase strictly awaits the user — `draft` (mark
 * ready) or `approved` (merge). `awaiting-approval` is excluded on purpose: the
 * PR is waiting on reviewers, not on a concrete user action.
 */
export function prPhaseNeedsYou(t: { tags?: string[]; prPhase?: string }): boolean {
  return isOwnPRTask(t) && PR_PHASE_NEEDS_YOU.has(prPhaseOf(t))
}
