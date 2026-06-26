export type TaskStatus =
  | 'new'
  | 'todo'
  | 'planning'
  | 'plan-review'
  | 'in-progress'
  | 'ready-review'
  | 'in-review'
  | 'testing'
  | 'ready-pr'
  | 'human-required'
  | 'blocked'
  | 'done'
  | 'cancelled'

export interface StatusMeta {
  value: TaskStatus
  label: string
  badgeClasses: string
  pillClasses: string
}

export type StatusOption<T extends string = string> = {
  value: T
  label: string
}

/** All valid statuses — mirrors Go internal/task/model.go */
export const ALL_STATUSES: StatusMeta[] = [
  {
    value: 'new',
    label: 'New',
    badgeClasses: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200',
    pillClasses: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200',
  },
  {
    value: 'todo',
    label: 'Todo',
    badgeClasses: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200',
    pillClasses: 'bg-surface-200 dark:bg-surface-700',
  },
  {
    value: 'planning',
    label: 'Planning',
    badgeClasses: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200',
    pillClasses: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200',
  },
  {
    value: 'plan-review',
    label: 'Plan Review',
    badgeClasses: 'bg-tertiary-100 text-tertiary-700 dark:bg-tertiary-800 dark:text-tertiary-300',
    pillClasses: 'bg-tertiary-100 text-tertiary-700 dark:bg-tertiary-800 dark:text-tertiary-300',
  },
  {
    value: 'in-progress',
    label: 'In Progress',
    badgeClasses: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200',
    pillClasses: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200',
  },
  {
    value: 'ready-review',
    label: 'Agentic Review',
    badgeClasses: 'bg-success-100 text-success-700 dark:bg-success-800 dark:text-success-300',
    pillClasses: 'bg-success-100 text-success-700 dark:bg-success-800 dark:text-success-300',
  },
  {
    value: 'in-review',
    label: 'In Review',
    badgeClasses: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
    pillClasses: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
  },
  {
    value: 'testing',
    label: 'Testing',
    badgeClasses: 'bg-secondary-200 text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200',
    pillClasses: 'bg-secondary-200 text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200',
  },
  {
    value: 'ready-pr',
    label: 'Opening PR',
    badgeClasses: 'bg-success-100 text-success-700 dark:bg-success-800 dark:text-success-300',
    pillClasses: 'bg-success-100 text-success-700 dark:bg-success-800 dark:text-success-300',
  },
  {
    value: 'human-required',
    label: 'Human Required',
    badgeClasses: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200',
    pillClasses: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200',
  },
  {
    value: 'blocked',
    label: 'Blocked',
    badgeClasses: 'bg-error-100 text-error-700 dark:bg-error-800 dark:text-error-300',
    pillClasses: 'bg-error-100 text-error-700 dark:bg-error-800 dark:text-error-300',
  },
  {
    value: 'done',
    label: 'Done',
    badgeClasses: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200',
    pillClasses: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200',
  },
  {
    value: 'cancelled',
    label: 'Cancelled',
    badgeClasses: 'bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400',
    pillClasses: 'bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400',
  },
]

/**
 * Statuses that await the user's own action — distinct from agent-driven
 * review states (`in-review`, `ready-review`) where an agent is working.
 * Mirrors orchestrator/CLAUDE.md: plan-review waits for the human to
 * approve/reject; human-required/blocked need human input.
 */
export const AWAITS_HUMAN: ReadonlySet<TaskStatus> = new Set<TaskStatus>([
  'human-required',
  'plan-review',
  'blocked',
])

/** True when a task is waiting on the user (not on an agent). */
export function awaitsHuman(status: string): boolean {
  return AWAITS_HUMAN.has(status as TaskStatus)
}

/**
 * Canonical, human-facing label for a status — the label every status surface
 * (board pill, list cell, move popover) should resolve through so one state
 * reads the same wherever it appears. Unknown values pass through verbatim so
 * the UI never mislabels a legacy/unrecognised status.
 */
export function statusLabel(status: string): string {
  return STATUS_MAP[status]?.label ?? status
}

/**
 * Label for an awaits-human task's attention pill; empty when not awaiting.
 * Returns the *canonical* status label — the attention state is signalled by
 * colour and placement, never by a divergent name. (Previously this invented
 * "Needs Review"/"Needs You", which is exactly the vocabulary split this
 * reconciles.)
 */
export function awaitsHumanLabel(status: string): string {
  return awaitsHuman(status) ? statusLabel(status) : ''
}

/** O(1) lookup by status value */
export const STATUS_MAP: Record<string, StatusMeta> = Object.fromEntries(
  ALL_STATUSES.map((s) => [s.value, s]),
)

export interface BoardColumn {
  status: TaskStatus
  label: string
  border: string
  /** Extra statuses folded into this column */
  includes: TaskStatus[]
  /**
   * Column kind. 'status' (default) groups tasks by status; 'review' is the
   * tag-based To Review lane that collects inbound review tasks across every
   * phase, independent of their status. `status` is a sentinel for review
   * lanes — never a real task status.
   */
  kind?: 'status' | 'review'
}

/** Kanban board columns — active work only; terminal tasks live in Logbook */
export const BOARD_COLUMNS: BoardColumn[] = [
  { status: 'todo', label: 'Todo', border: 'border-t-surface-400 dark:border-t-surface-500', includes: ['new', 'todo'] },
  { status: 'planning', label: 'Planning', border: 'border-t-tertiary-500 dark:border-t-tertiary-400', includes: ['planning', 'plan-review'] },
  { status: 'in-progress', label: 'In Progress', border: 'border-t-primary-500 dark:border-t-primary-400', includes: [] },
  { status: 'ready-review', label: 'Agentic Review', border: 'border-t-success-500 dark:border-t-success-400', includes: [] },
  { status: 'testing', label: 'Testing', border: 'border-t-secondary-500 dark:border-t-secondary-400', includes: ['testing'] },
  { status: 'in-review', label: 'In Review', border: 'border-t-warning-500 dark:border-t-warning-400', includes: ['in-review', 'ready-pr'] },
  { status: 'human-required', label: 'Human Required', border: 'border-t-error-500 dark:border-t-error-400', includes: ['human-required', 'blocked'] },
]

/**
 * The To Review lane: a tag-based column (not status-based) that collects
 * every inbound review task (tag `review`) regardless of status, coloured per
 * phase. `status: 'reviews'` is a sentinel used only as the column key — it is
 * never a real task status, so it stays out of BOARD_COLUMNS / CORE_STATUSES.
 */
export const REVIEW_LANE: BoardColumn = {
  status: 'reviews' as TaskStatus,
  kind: 'review',
  label: 'To Review',
  border: 'border-t-secondary-500 dark:border-t-secondary-400',
  includes: [],
}

/**
 * Board column order including the To Review lane (inserted before Human
 * Required). The board renders these; BOARD_COLUMNS stays the pure status set
 * that drives CORE_STATUSES, the status picker, and per-project boards.
 */
export const BOARD_LANES: BoardColumn[] = BOARD_COLUMNS.flatMap((c) =>
  c.status === 'human-required' ? [REVIEW_LANE, c] : [c],
)

/**
 * Core user-facing status set: the active board columns plus the terminal
 * states (`done`, `cancelled`). Granular states (new, plan-review,
 * ready-pr, blocked) are internal/derived — set by automations, not
 * picked by hand. Users choose from this small set so a task's status always
 * lines up with a board column.
 */
export const CORE_STATUSES: TaskStatus[] = [
  ...BOARD_COLUMNS.map((c) => c.status),
  'done',
  'cancelled',
]

const CORE_STATUS_SET: ReadonlySet<TaskStatus> = new Set(CORE_STATUSES)

/** Core statuses as dropdown/picker options. */
export const CORE_STATUS_OPTIONS: StatusOption<TaskStatus>[] = CORE_STATUSES.map(
  (value) => ({ value, label: STATUS_MAP[value].label }),
)

function isCoreStatus(status: string): status is TaskStatus {
  return CORE_STATUS_SET.has(status as TaskStatus)
}

/** Granular status → the core (column) status it rolls up to. */
const STATUS_TO_CORE: Record<string, TaskStatus> = (() => {
  const map: Record<string, TaskStatus> = {}
  for (const col of BOARD_COLUMNS) {
    map[col.status] = col.status
    for (const folded of col.includes) map[folded] = col.status
  }
  return map
})()

/** Roll any status up to its core (column) status; terminal states map to self. */
export function coreStatus(status: TaskStatus): TaskStatus
export function coreStatus(status: string): string
export function coreStatus(status: string): string {
  return STATUS_TO_CORE[status] ?? status
}

/**
 * Picker options for a task whose current status is `current`. Normally the
 * core set, but if `current` rolls up to something outside the core set (an
 * unknown/legacy status), its own value is appended so the control never
 * mislabels or silently reassigns it.
 */
export function statusOptionsFor(current: TaskStatus): StatusOption<TaskStatus>[]
export function statusOptionsFor(current: string): StatusOption<string>[]
export function statusOptionsFor(current: string): StatusOption<string>[] {
  const core = coreStatus(current)
  if (isCoreStatus(core)) return CORE_STATUS_OPTIONS
  return [...CORE_STATUS_OPTIONS, { value: core, label: STATUS_MAP[core]?.label ?? core }]
}
