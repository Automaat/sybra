export type TaskStatus =
  | 'new'
  | 'todo'
  | 'planning'
  | 'plan-review'
  | 'in-progress'
  | 'in-review'
  | 'human-required'
  | 'done'

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
    value: 'in-review',
    label: 'In Review',
    badgeClasses: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
    pillClasses: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
  },
  {
    value: 'human-required',
    label: 'Human Required',
    badgeClasses: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200',
    pillClasses: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200',
  },
  {
    value: 'done',
    label: 'Done',
    badgeClasses: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200',
    pillClasses: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200',
  },
]

/** O(1) lookup by status value */
export const STATUS_MAP: Record<string, StatusMeta> = Object.fromEntries(
  ALL_STATUSES.map((s) => [s.value, s]),
)

/** For dropdowns / segmented controls */
export const STATUS_OPTIONS: { value: TaskStatus; label: string }[] = ALL_STATUSES.map(
  ({ value, label }) => ({ value, label }),
)

export interface BoardColumn {
  status: TaskStatus
  label: string
  border: string
  /** Extra statuses folded into this column */
  includes: TaskStatus[]
}

/** Kanban board columns — used by TaskList and ProjectDetail */
export const BOARD_COLUMNS: BoardColumn[] = [
  { status: 'todo', label: 'Todo', border: 'border-t-surface-400 dark:border-t-surface-500', includes: ['new', 'todo'] },
  { status: 'planning', label: 'Planning', border: 'border-t-tertiary-500 dark:border-t-tertiary-400', includes: ['planning', 'plan-review'] },
  { status: 'in-progress', label: 'In Progress', border: 'border-t-primary-500 dark:border-t-primary-400', includes: [] },
  { status: 'in-review', label: 'In Review', border: 'border-t-warning-500 dark:border-t-warning-400', includes: [] },
  { status: 'human-required', label: 'Human Required', border: 'border-t-error-500 dark:border-t-error-400', includes: [] },
  { status: 'done', label: 'Done', border: 'border-t-success-500 dark:border-t-success-400', includes: [] },
]
