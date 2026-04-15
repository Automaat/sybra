<script lang="ts">
  import { agentStore } from '../stores/agents.svelte.js'

  interface ErrorEvent {
    kind: string
    msg: string
  }

  interface Props {
    agentId: string
    error: ErrorEvent
    onretry?: () => void
    ondismiss?: () => void
  }

  const { agentId, error, onretry, ondismiss }: Props = $props()

  let logsExpanded = $state(false)

  type ErrorSpec = {
    label: string
    icon: string
    what: string
    todo: string
    color: string
    iconColor: string
  }

  const ERROR_SPECS: Record<string, ErrorSpec> = {
    worktree_conflict: {
      label: 'Worktree conflict',
      icon: `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M7 16V4m0 0L3 8m4-4l4 4m6 0v12m0 0l4-4m-4 4l-4-4" />`,
      what: 'Git worktree is already checked out by another agent.',
      todo: 'Stop the conflicting agent or retry once it finishes.',
      color: 'bg-error-50 border-error-400 dark:bg-error-950 dark:border-error-600',
      iconColor: 'text-error-500',
    },
    git_clone: {
      label: 'Git / network error',
      icon: `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 15a4 4 0 004 4h9a5 5 0 10-.1-9.999 5.002 5.002 0 10-9.78 2.096A4.001 4.001 0 003 15z" />`,
      what: 'Failed to clone or fetch the repository.',
      todo: 'Check network connectivity and repository access, then retry.',
      color: 'bg-warning-50 border-warning-400 dark:bg-warning-950 dark:border-warning-600',
      iconColor: 'text-warning-500',
    },
    permission_denied: {
      label: 'Permission denied',
      icon: `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M18.364 18.364A9 9 0 005.636 5.636m12.728 12.728A9 9 0 015.636 5.636m12.728 12.728L5.636 5.636" />`,
      what: 'A tool call was rejected due to insufficient permissions.',
      todo: 'Review allowed tools in task settings and retry.',
      color: 'bg-error-50 border-error-400 dark:bg-error-950 dark:border-error-600',
      iconColor: 'text-error-500',
    },
    rate_limit: {
      label: 'API rate limited',
      icon: `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8v4l3 3m6-3a9 9 0 11-18 0 9 9 0 0118 0z" />`,
      what: 'The API provider rate limit was reached.',
      todo: 'Wait a few minutes and retry, or check provider health.',
      color: 'bg-warning-50 border-warning-400 dark:bg-warning-950 dark:border-warning-600',
      iconColor: 'text-warning-500',
    },
    crash: {
      label: 'Agent crashed',
      icon: `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />`,
      what: 'The agent process exited unexpectedly.',
      todo: 'View logs for details, then retry.',
      color: 'bg-error-50 border-error-400 dark:bg-error-950 dark:border-error-600',
      iconColor: 'text-error-500',
    },
  }

  const spec = $derived(ERROR_SPECS[error.kind] ?? ERROR_SPECS.crash)
  const logs = $derived(agentStore.outputs.get(agentId) ?? [])
</script>

<div class="rounded-lg border-2 {spec.color} p-4">
  <div class="flex items-start gap-3">
    <svg class="h-5 w-5 shrink-0 {spec.iconColor} mt-0.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
      {@html spec.icon}
    </svg>
    <div class="min-w-0 flex-1">
      <div class="flex flex-wrap items-center gap-2">
        <span class="text-sm font-semibold text-surface-800 dark:text-surface-100">{spec.label}</span>
        <span class="rounded bg-surface-200 px-1.5 py-0.5 font-mono text-xs text-surface-600 dark:bg-surface-700 dark:text-surface-400">{error.kind}</span>
      </div>
      <p class="mt-1 text-sm text-surface-700 dark:text-surface-300">{spec.what}</p>
      <p class="text-sm text-surface-500 dark:text-surface-400">{spec.todo}</p>
      {#if error.msg && error.msg !== spec.what}
        <p class="mt-1 truncate font-mono text-xs text-surface-500 dark:text-surface-400" title={error.msg}>{error.msg}</p>
      {/if}
    </div>
  </div>

  <div class="mt-3 flex flex-wrap items-center gap-2">
    {#if onretry}
      <button
        type="button"
        class="flex items-center gap-1.5 rounded-md bg-surface-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-surface-800 dark:bg-surface-600 dark:hover:bg-surface-500"
        onclick={onretry}
      >
        <svg class="h-3.5 w-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
        </svg>
        Retry
      </button>
    {/if}
    {#if logs.length > 0}
      <button
        type="button"
        class="flex items-center gap-1.5 rounded-md border border-surface-300 px-3 py-1.5 text-sm font-medium text-surface-700 hover:bg-surface-100 dark:border-surface-600 dark:text-surface-300 dark:hover:bg-surface-800"
        onclick={() => (logsExpanded = !logsExpanded)}
      >
        <svg class="h-3.5 w-3.5 transition-transform {logsExpanded ? 'rotate-90' : ''}" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7" />
        </svg>
        View logs
      </button>
    {/if}
    {#if ondismiss}
      <button
        type="button"
        class="ml-auto flex items-center gap-1 text-sm text-surface-400 hover:text-surface-600 dark:hover:text-surface-200"
        onclick={ondismiss}
      >
        Dismiss
      </button>
    {/if}
  </div>

  {#if logsExpanded && logs.length > 0}
    <div class="mt-3 max-h-48 overflow-y-auto rounded bg-surface-900 p-3 font-mono text-xs text-surface-200">
      {#each logs as ev}
        {#if ev.content}
          <div class="mb-0.5 leading-relaxed">{ev.content}</div>
        {/if}
      {/each}
    </div>
  {/if}
</div>
