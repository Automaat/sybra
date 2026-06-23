export type TaskStatus =
  | 'new'
  | 'todo'
  | 'planning'
  | 'plan-review'
  | 'in-progress'
  | 'ready-review'
  | 'in-review'
  | 'testing'
  | 'test-plan-review'
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
    label: 'Ready for Review',
    badgeClasses: 'bg-warning-100 text-warning-700 dark:bg-warning-800 dark:text-warning-300',
    pillClasses: 'bg-warning-100 text-warning-700 dark:bg-warning-800 dark:text-warning-300',
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
    value: 'test-plan-review',
    label: 'Test Plan Review',
    badgeClasses: 'bg-secondary-100 text-secondary-700 dark:bg-secondary-800 dark:text-secondary-300',
    pillClasses: 'bg-secondary-100 text-secondary-700 dark:bg-secondary-800 dark:text-secondary-300',
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
 * Mirrors orchestrator/CLAUDE.md: plan-review/test-plan-review wait for the
 * human to approve/reject; human-required/blocked need human input.
 */
export const AWAITS_HUMAN: ReadonlySet<TaskStatus> = new Set<TaskStatus>([
  'human-required',
  'plan-review',
  'test-plan-review',
  'blocked',
])

/** True when a task is waiting on the user (not on an agent). */
export function awaitsHuman(status: string): boolean {
  return AWAITS_HUMAN.has(status as TaskStatus)
}

/** Short pill label for an awaits-human task; empty when not awaiting. */
export function awaitsHumanLabel(status: string): string {
  switch (status) {
    case 'plan-review':
    case 'test-plan-review':
      return 'Needs Review'
    case 'blocked':
      return 'Blocked'
    case 'human-required':
      return 'Needs You'
    default:
      return ''
  }
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
}

/** Kanban board columns — active work only; terminal tasks live in Logbook */
export const BOARD_COLUMNS: BoardColumn[] = [
  { status: 'todo', label: 'Todo', border: 'border-t-surface-400 dark:border-t-surface-500', includes: ['new', 'todo'] },
  { status: 'planning', label: 'Planning', border: 'border-t-tertiary-500 dark:border-t-tertiary-400', includes: ['planning', 'plan-review'] },
  { status: 'in-progress', label: 'In Progress', border: 'border-t-primary-500 dark:border-t-primary-400', includes: [] },
  { status: 'in-review', label: 'In Review', border: 'border-t-warning-500 dark:border-t-warning-400', includes: ['in-review', 'ready-review'] },
  { status: 'testing', label: 'Testing', border: 'border-t-secondary-500 dark:border-t-secondary-400', includes: ['testing', 'test-plan-review'] },
  { status: 'human-required', label: 'Human Required', border: 'border-t-error-500 dark:border-t-error-400', includes: ['human-required', 'blocked'] },
]

/**
 * Core user-facing status set: the active board columns plus `done`. Granular
 * states (new, plan-review, ready-review, test-plan-review, blocked, cancelled)
 * are internal/derived — set by automations, not picked by hand. Users choose
 * from this small set so a task's status always lines up with a board column.
 */
export const CORE_STATUSES: TaskStatus[] = [
  ...BOARD_COLUMNS.map((c) => c.status),
  'done',
  'cancelled',
]

/** Core statuses as dropdown/picker options. */
export const CORE_STATUS_OPTIONS: { value: TaskStatus; label: string }[] = CORE_STATUSES.map(
  (value) => ({ value, label: STATUS_MAP[value].label }),
)

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
export function coreStatus(status: string): TaskStatus {
  return STATUS_TO_CORE[status] ?? (status as TaskStatus)
}

/**
 * Picker options for a task whose current status is `current`. Normally the
 * core set, but if `current` rolls up to something outside the core set (an
 * unknown/legacy status), its own value is appended so the control never
 * mislabels or silently reassigns it.
 */
export function statusOptionsFor(current: string): { value: TaskStatus; label: string }[] {
  const core = coreStatus(current)
  if (CORE_STATUSES.includes(core)) return CORE_STATUS_OPTIONS
  return [...CORE_STATUS_OPTIONS, { value: core, label: STATUS_MAP[core]?.label ?? core }]
}
