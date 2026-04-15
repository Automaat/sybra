<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { fade } from 'svelte/transition'
  import { getAgentPhase, PHASE_CONFIG } from '$lib/agent-phases.js'

  interface Props {
    agent: agent.Agent
    onclick: () => void
  }

  const { agent: a, onclick }: Props = $props()

  const linkedTask = $derived(a.taskId ? taskStore.tasks.get(a.taskId) : null)

  const phase = $derived(
    getAgentPhase(
      a.state,
      a.escalationReason,
      linkedTask?.status,
      a.awaitingApproval,
    ),
  )
  const config = $derived(PHASE_CONFIG[phase])

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
  class="w-full rounded-lg border border-surface-300 bg-surface-50 p-4 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700 {config.faded ? 'opacity-60' : ''}"
  onclick={onclick}
>
  <div class="mb-2 flex items-start justify-between gap-2">
    <div class="flex flex-col gap-0.5">
      <h3 class="text-sm font-semibold leading-tight {config.faded ? 'text-surface-400 dark:text-surface-500' : ''}">{a.project || a.id}</h3>
      {#if a.name}
        <span class="text-xs text-surface-400">{a.name}</span>
      {/if}
      {#if phase === 'running'}
        <span class="text-xs italic text-surface-400">
          {agentStore.stepTexts.get(a.id) ?? 'Working...'}
        </span>
      {:else if phase === 'human-required'}
        <span class="text-xs font-medium text-error-600 dark:text-error-400">
          Waiting for human input
        </span>
      {:else if phase === 'waiting'}
        <span class="text-xs text-surface-400">
          Waiting for reply
        </span>
      {:else if phase === 'blocked'}
        <span class="text-xs text-tertiary-600 dark:text-tertiary-400">
          Awaiting tool approval
        </span>
      {/if}
    </div>
    <span class="inline-flex items-center gap-1 rounded-full px-2.5 py-0.5 text-xs font-medium transition-all duration-150 {config.badgeClasses}">
      {#if config.animate}
        <span transition:fade={{ duration: 150 }} class="h-1.5 w-1.5 animate-pulse-subtle rounded-full {config.dotClasses}"></span>
      {:else if phase !== 'queued' && phase !== 'done'}
        <span transition:fade={{ duration: 150 }} class="h-1.5 w-1.5 rounded-full {config.dotClasses}"></span>
      {/if}
      {config.label}
    </span>
  </div>

  {#if phase === 'human-required'}
    <div class="mb-2 rounded border border-error-300 bg-error-50 px-2.5 py-1.5 text-xs text-error-700 dark:border-error-700 dark:bg-error-950 dark:text-error-300">
      Action required — open agent to respond
    </div>
  {/if}

  <div class="flex items-center gap-2 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{a.mode}</span>
    {#if a.external}
      <span class="rounded bg-warning-200 px-1.5 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">external</span>
    {/if}
    {#if a.project}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{a.project}</span>
    {/if}
    {#if a.taskId}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{linkedTask?.title ?? a.taskId}</span>
    {/if}
    {#if linkedTask?.branch}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 font-mono dark:bg-surface-700">{linkedTask.branch.replace(/^synapse\//, '')}</span>
    {/if}
    {#if a.costUsd > 0}
      <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">${a.costUsd.toFixed(2)}</span>
    {/if}
    <span class="ml-auto opacity-60">{timeAgo(a.startedAt)}</span>
  </div>
</button>
