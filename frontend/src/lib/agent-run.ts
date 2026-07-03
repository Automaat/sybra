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

// Human-friendly label for an agent run's role (what the agent was doing).
// The backend persists the role prefix from the agent name; "" and
// "implementation" are the same thing. Returns "" for roles we don't want to
// surface as a badge (plain implementation carries no extra signal).
export function roleLabel(role: string | undefined): string {
  switch (role) {
    case 'triage':
      return 'Triage'
    case 'plan':
      return 'Plan'
    case 'plan-critic':
      return 'Plan Critic'
    case 'eval':
      return 'Eval'
    case 'pr-fix':
      return 'PR Fix'
    case 'review':
      return 'Review'
    case 'fix-review':
      return 'Fix Review'
    case 'test-runner':
      return 'Testing'
    case 'human-review':
      return 'Human Review'
    case 'chat':
      return 'Chat'
    default: // "" / "implementation" / unknown — no distinct role signal
      return ''
  }
}
