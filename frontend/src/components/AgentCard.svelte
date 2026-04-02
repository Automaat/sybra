<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'

  interface Props {
    agent: agent.Agent
    onclick: () => void
  }

  const { agent: a, onclick }: Props = $props()

  const stateConfig: Record<string, { label: string; classes: string }> = {
    idle: { label: 'Idle', classes: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' },
    running: { label: 'Running', classes: 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' },
    paused: { label: 'Waiting', classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' },
    stopped: { label: 'Stopped', classes: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' },
  }

  const resolved = $derived(stateConfig[a.state] ?? { label: a.state, classes: 'bg-surface-200 text-surface-800' })

  function timeAgo(date: any): string {
    if (!date) return ''
    const now = Date.now()
    const then = new Date(date).getTime()
    const diff = Math.floor((now - then) / 1000)
    if (diff < 60) return 'just now'
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
    return `${Math.floor(diff / 86400)}d ago`
  }
</script>

<button
  type="button"
  class="w-full rounded-lg border border-surface-300 bg-surface-50 p-4 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
  onclick={onclick}
>
  <div class="mb-2 flex items-start justify-between gap-2">
    <div class="flex flex-col gap-0.5">
      <h3 class="text-sm font-semibold leading-tight">{a.project || a.id}</h3>
      {#if a.name}
        <span class="text-xs text-surface-400">{a.name}</span>
      {/if}
    </div>
    <span class="inline-flex items-center gap-1 rounded-full px-2.5 py-0.5 text-xs font-medium {resolved.classes}">
      {#if a.state === 'running'}
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-success-500"></span>
      {:else if a.state === 'paused'}
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
      {/if}
      {resolved.label}
    </span>
  </div>

  <div class="flex items-center gap-2 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{a.mode}</span>
    {#if a.external}
      <span class="rounded bg-warning-200 px-1.5 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">external</span>
    {/if}
    {#if a.project}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{a.project}</span>
    {/if}
    {#if a.taskId}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">task: {a.taskId}</span>
    {/if}
    {#if a.costUsd > 0}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">${a.costUsd.toFixed(2)}</span>
    {/if}
    <span class="ml-auto opacity-60">{timeAgo(a.startedAt)}</span>
  </div>
</button>
