<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { fade } from 'svelte/transition'

  interface Props {
    agent: agent.Agent
    onclick: () => void
  }

  const { agent: a, onclick }: Props = $props()

  const stateConfig: Record<string, { label: string; classes: string }> = {
    idle: { label: 'Idle', classes: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' },
    running: { label: 'Running', classes: 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' },
    paused: { label: 'Waiting', classes: 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' },
    stopped: { label: 'Stopped', classes: 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' },
  }

  const resolved = $derived(
    a.errorKind
      ? { label: 'Error', classes: 'bg-error-200 text-error-800 dark:bg-error-800 dark:text-error-100' }
      : (stateConfig[a.state] ?? { label: a.state, classes: 'bg-surface-200 text-surface-800' })
  )

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
  class="w-full rounded-lg border p-4 text-left transition-colors hover:bg-surface-100 dark:hover:bg-surface-700
    {a.errorKind
      ? 'border-error-400 bg-error-50 dark:border-error-600 dark:bg-error-950/30'
      : 'border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-800'}"
  onclick={onclick}
>
  <div class="mb-2 flex items-start justify-between gap-2">
    <div class="flex flex-col gap-0.5">
      <h3 class="text-sm font-semibold leading-tight">{a.project || a.id}</h3>
      {#if a.name}
        <span class="text-xs text-surface-400">{a.name}</span>
      {/if}
      {#if a.state === 'running'}
        <span class="text-xs italic text-surface-400">
          {agentStore.stepTexts.get(a.id) ?? 'Working...'}
        </span>
      {/if}
    </div>
    <span class="inline-flex items-center gap-1 rounded-full px-2.5 py-0.5 text-xs font-medium transition-all duration-150 {resolved.classes}">
      {#if a.errorKind}
        <svg class="h-3 w-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
        </svg>
      {:else if a.state === 'running'}
        <span transition:fade={{ duration: 150 }} class="h-1.5 w-1.5 animate-pulse-subtle rounded-full bg-success-500"></span>
      {:else if a.state === 'paused'}
        <span transition:fade={{ duration: 150 }} class="h-1.5 w-1.5 animate-pulse-subtle rounded-full bg-warning-500"></span>
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
