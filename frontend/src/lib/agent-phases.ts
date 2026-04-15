export type AgentPhase =
  | 'queued'
  | 'running'
  | 'waiting'
  | 'blocked'
  | 'human-required'
  | 'reviewing'
  | 'done'

export interface PhaseConfig {
  phase: AgentPhase
  label: string
  /** Tailwind classes for the dot indicator */
  dotClasses: string
  /** Tailwind classes for the badge pill */
  badgeClasses: string
  /** Whether the dot should pulse */
  animate: boolean
  /** Whether the row should be visually de-emphasised */
  faded: boolean
}

export const PHASE_CONFIG: Record<AgentPhase, PhaseConfig> = {
  queued: {
    phase: 'queued',
    label: 'Queued',
    dotClasses: 'bg-surface-400 dark:bg-surface-500',
    badgeClasses: 'bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400',
    animate: false,
    faded: false,
  },
  waiting: {
    phase: 'waiting',
    label: 'Waiting',
    dotClasses: 'bg-surface-400 dark:bg-surface-500',
    badgeClasses: 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-300',
    animate: false,
    faded: false,
  },
  running: {
    phase: 'running',
    label: 'Running',
    dotClasses: 'bg-primary-500',
    badgeClasses: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200',
    animate: true,
    faded: false,
  },
  blocked: {
    phase: 'blocked',
    label: 'Blocked',
    // tertiary = warm amber — distinct from primary amber, matches "amber dot" spec
    dotClasses: 'bg-tertiary-500',
    badgeClasses: 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200',
    animate: false,
    faded: false,
  },
  'human-required': {
    phase: 'human-required',
    label: 'Needs Input',
    // error = coral/orange — matches "orange dot" spec
    dotClasses: 'bg-error-500',
    badgeClasses: 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200',
    animate: true,
    faded: false,
  },
  reviewing: {
    phase: 'reviewing',
    label: 'Reviewing',
    // warning = violet/purple in amber theme — matches "purple dot" spec
    dotClasses: 'bg-warning-500',
    badgeClasses: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200',
    animate: false,
    faded: false,
  },
  done: {
    phase: 'done',
    label: 'Done',
    dotClasses: 'bg-success-400',
    badgeClasses: 'bg-success-100 text-success-700 dark:bg-success-900 dark:text-success-300',
    animate: false,
    faded: true,
  },
}

/**
 * Derive the visual phase for an agent from its state and context.
 *
 * - idle                                          → queued
 * - running                                       → running
 * - paused, has escalationReason                  → human-required (guardrail hit)
 * - paused, awaitingApproval=true                 → blocked (tool approval pending)
 * - paused, neither                               → waiting (conversational, awaiting next message)
 * - stopped, task status = in-review              → reviewing
 * - stopped                                       → done
 */
export function getAgentPhase(
  state: string,
  escalationReason?: string,
  taskStatus?: string,
  awaitingApproval?: boolean,
): AgentPhase {
  switch (state) {
    case 'idle':
      return 'queued'
    case 'running':
      return 'running'
    case 'paused':
      if (escalationReason) return 'human-required'
      if (awaitingApproval) return 'blocked'
      return 'waiting'
    case 'stopped':
      if (taskStatus === 'in-review') return 'reviewing'
      return 'done'
    default:
      return 'done'
  }
}
