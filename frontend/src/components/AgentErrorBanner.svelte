<script lang="ts">
  import type { Component } from 'svelte'
  import { ArrowUpDown, Cloud, Ban, Clock, AlertTriangle, RotateCcw, ChevronRight } from '@lucide/svelte'
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
    icon: Component<{ size?: number }>
    what: string
    todo: string
    color: string
    iconColor: string
  }

  const ERROR_SPECS: Record<string, ErrorSpec> = {
    worktree_conflict: {
      label: 'Worktree conflict',
      icon: ArrowUpDown,
      what: 'Git worktree is already checked out by another agent.',
      todo: 'Stop the conflicting agent or retry once it finishes.',
      color: 'bg-error-50 border-error-400 dark:bg-error-950 dark:border-error-600',
      iconColor: 'text-error-500',
    },
    git_clone: {
      label: 'Git / network error',
      icon: Cloud,
      what: 'Failed to clone or fetch the repository.',
      todo: 'Check network connectivity and repository access, then retry.',
      color: 'bg-warning-50 border-warning-400 dark:bg-warning-950 dark:border-warning-600',
      iconColor: 'text-warning-500',
    },
    permission_denied: {
      label: 'Permission denied',
      icon: Ban,
      what: 'A tool call was rejected due to insufficient permissions.',
      todo: 'Review allowed tools in task settings and retry.',
      color: 'bg-error-50 border-error-400 dark:bg-error-950 dark:border-error-600',
      iconColor: 'text-error-500',
    },
    rate_limit: {
      label: 'API rate limited',
      icon: Clock,
      what: 'The API provider rate limit was reached.',
      todo: 'Wait a few minutes and retry, or check provider health.',
      color: 'bg-warning-50 border-warning-400 dark:bg-warning-950 dark:border-warning-600',
      iconColor: 'text-warning-500',
    },
    crash: {
      label: 'Agent crashed',
      icon: AlertTriangle,
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
    <span class="mt-0.5 shrink-0 {spec.iconColor}">
      <spec.icon size={20} />
    </span>
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
        <RotateCcw size={14} />
        Retry
      </button>
    {/if}
    {#if logs.length > 0}
      <button
        type="button"
        class="flex items-center gap-1.5 rounded-md border border-surface-300 px-3 py-1.5 text-sm font-medium text-surface-700 hover:bg-surface-100 dark:border-surface-600 dark:text-surface-300 dark:hover:bg-surface-800"
        onclick={() => (logsExpanded = !logsExpanded)}
      >
        <ChevronRight size={14} class="transition-transform {logsExpanded ? 'rotate-90' : ''}" />
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
        {#if ev.event.content}
          <div class="mb-0.5 leading-relaxed">{ev.event.content}</div>
        {/if}
      {/each}
    </div>
  {/if}
</div>
