// Treatment for an agent run's state pill in the history list.
//
// A finished run is inert, so it must never wear the amber action colour. The
// backend persists "stopped" for every finished headless run (it doesn't record
// success vs failure separately), so that — the common real case — is neutral
// grey: honest, and no longer the orange alarm. Only a live "running" run gets
// amber. Explicit outcome labels ("completed"/"failed") that may appear in
// hand-authored or future data map to muted green/coral rather than orange.
export function runStateClasses(state: string): string {
  switch (state) {
    case 'completed':
    case 'done':
    case 'success':
      return 'bg-success-100 text-success-700 dark:bg-success-900/50 dark:text-success-300'
    case 'failed':
    case 'error':
      return 'bg-error-100 text-error-700 dark:bg-error-900/50 dark:text-error-300'
    case 'running':
      return 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200'
    default: // stopped (the usual finished state), paused, idle, unknown
      return 'bg-surface-200 text-surface-700 dark:bg-surface-700 dark:text-surface-300'
  }
}

// Human-readable label for an agent run's pipeline role (AgentRun.role). Roles
// are the lowercase technical values the backend persists (see
// internal/agent/role.go and lib/agent-name.ts). An empty/absent role carries
// no reliable meaning — legacy implementation runs and un-tagged runs both look
// like "" — so it returns '' and the caller renders no badge rather than
// guessing "Implementation".
export function runRoleLabel(role: string | undefined | null): string {
  switch (role) {
    case 'triage': return 'Triage'
    case 'plan': return 'Plan'
    case 'plan-critic': return 'Plan Critic'
    case 'implementation': return 'Implementation'
    case 'review': return 'Review'
    case 'fix-review': return 'Fix Review'
    case 'pr-fix': return 'PR Fix'
    case 'test-plan': return 'Test Plan'
    case 'test-plan-critic': return 'Test Plan Critic'
    case 'test-runner': return 'Test'
    case 'eval': return 'Eval'
    case 'human-review': return 'Human Review'
    case 'chat': return 'Chat'
    default: return role ? role : ''
  }
}

// Colour for a run's role badge, grouped by pipeline phase so a glance reads the
// shape of the run history: planning (tertiary), implementation (primary),
// review/fix (warning), testing (secondary), everything else neutral. Distinct
// from runStateClasses — role answers "what kind of run", state answers "how it
// ended".
export function runRoleClasses(role: string | undefined | null): string {
  switch (role) {
    case 'triage':
    case 'plan':
    case 'plan-critic':
      return 'bg-tertiary-100 text-tertiary-700 dark:bg-tertiary-900/50 dark:text-tertiary-300'
    case 'implementation':
      return 'bg-primary-100 text-primary-700 dark:bg-primary-900/50 dark:text-primary-300'
    case 'review':
    case 'fix-review':
    case 'pr-fix':
      return 'bg-warning-100 text-warning-700 dark:bg-warning-900/50 dark:text-warning-300'
    case 'test-plan':
    case 'test-plan-critic':
    case 'test-runner':
      return 'bg-secondary-100 text-secondary-700 dark:bg-secondary-900/50 dark:text-secondary-300'
    default:
      return 'bg-surface-200 text-surface-700 dark:bg-surface-700 dark:text-surface-300'
  }
}
