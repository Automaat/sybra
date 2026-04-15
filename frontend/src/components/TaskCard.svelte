<script lang="ts">
  import { CheckCircle, XCircle, Clock, GitPullRequest, CircleDot } from '@lucide/svelte'
  import type { task } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { STATUS_OPTIONS } from '../lib/statuses.js'

  interface Props {
    task: task.Task
    onclick: () => void
    focused?: boolean
    onstatuschange?: (status: string) => void
  }

  const { task: t, onclick, focused = false, onstatuschange }: Props = $props()

  function handleStatusChange(e: Event) {
    e.stopPropagation()
    const value = (e.currentTarget as HTMLSelectElement).value
    if (value !== t.status) onstatuschange?.(value)
  }

  let dragging = $state(false)

  const triaging = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.name?.startsWith('triage:') && a.state === 'running')
  )

  const evaluating = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.name?.startsWith('eval:') && a.state === 'running')
  )

  const planning = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.name?.startsWith('plan:') && a.state === 'running')
  )

  const agentRunning = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.state === 'running' && !a.name?.startsWith('triage:') && !a.name?.startsWith('eval:') && !a.name?.startsWith('plan:'))
  )

  const hasRunningAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === t.id && a.state === 'running')
  )

  // Statuses that conflict with a running agent — cannot move to these while agent is active
  const AGENT_BLOCKED_STATUSES = new Set(['new', 'todo', 'done'])

  const linkedPRs = $derived(reviewStore.byTask(t))
  const topPR = $derived(linkedPRs.length > 0 ? linkedPRs[0] : null)

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

<div
  data-focused-task={focused ? '' : undefined}
  class="w-full select-none rounded-lg border bg-surface-50 p-3 text-left transition-all duration-100 active:bg-surface-100 dark:bg-surface-800 dark:active:bg-surface-700 md:hover:bg-surface-100 md:dark:hover:bg-surface-700 {focused ? 'border-primary-400 ring-2 ring-primary-400/50 dark:border-primary-500 dark:ring-primary-500/50' : 'border-surface-300 dark:border-surface-600'} {dragging ? 'opacity-40 shadow-lg' : ''}"
>
  <button
    type="button"
    draggable="true"
    onclick={onclick}
    ondragstart={(e) => {
      dragging = true
      e.dataTransfer!.setData('text/plain', t.id)
      e.dataTransfer!.effectAllowed = 'move'
    }}
    ondragend={() => { dragging = false }}
    class="flex w-full flex-col items-stretch gap-1.5 text-left"
  >
  <div class="mb-1.5 flex items-center gap-1.5">
    {#if topPR?.ciStatus === 'SUCCESS'}
      <CheckCircle size={16} class="shrink-0 text-success-500" />
    {:else if topPR?.ciStatus === 'FAILURE'}
      <XCircle size={16} class="shrink-0 text-error-500" />
    {:else if topPR?.ciStatus === 'PENDING'}
      <Clock size={16} class="shrink-0 text-warning-500" />
    {/if}
    <h3 class="text-sm font-semibold leading-tight">{t.title}</h3>
  </div>

  {#if t.statusReason}
    <p class="mb-1.5 line-clamp-2 text-xs text-warning-600 dark:text-warning-400">{t.statusReason}</p>
  {/if}

  <div class="flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
      {t.agentMode}
    </span>

    {#if t.projectId}
      <span class="rounded bg-primary-100 px-1.5 py-0.5 text-primary-700 dark:bg-primary-800 dark:text-primary-300">
        {t.projectId}
      </span>
    {/if}

    {#if t.branch}
      <span class="inline-flex items-center gap-1 rounded bg-surface-200 px-1.5 py-0.5 font-mono dark:bg-surface-700">
        <svg class="h-3 w-3 shrink-0" viewBox="0 0 16 16" fill="currentColor"><title>Branch</title><path d="M9.5 3.25a2.25 2.25 0 1 1 3 2.122V6A2.5 2.5 0 0 1 10 8.5H6a1 1 0 0 0-1 1v1.128a2.251 2.251 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.5 0v1.836A2.493 2.493 0 0 1 6 7h4a1 1 0 0 0 1-1v-.628A2.25 2.25 0 0 1 9.5 3.25Z"/></svg>
        {t.branch.replace(/^synapse\//, '')}
      </span>
    {/if}

    {#if triaging}
      <span class="inline-flex items-center gap-1 rounded bg-primary-200 px-1.5 py-0.5 text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        <span class="h-1.5 w-1.5 animate-pulse-subtle rounded-full bg-primary-500"></span>
        Triaging
      </span>
    {/if}

    {#if planning}
      <span class="inline-flex items-center gap-1 rounded bg-tertiary-200 px-1.5 py-0.5 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        <span class="h-1.5 w-1.5 animate-pulse-subtle rounded-full bg-tertiary-500"></span>
        Planning
      </span>
    {/if}

    {#if t.status === 'plan-review'}
      <span class="inline-flex items-center gap-1 rounded bg-error-200 px-1.5 py-0.5 font-semibold text-error-800 dark:bg-error-700 dark:text-error-200">
        Needs Review
      </span>
    {/if}

    {#if agentRunning}
      <span class="inline-flex items-center gap-1 rounded bg-success-200 px-1.5 py-0.5 text-success-800 dark:bg-success-700 dark:text-success-200">
        <span class="h-1.5 w-1.5 animate-pulse-subtle rounded-full bg-success-500"></span>
        Agent
      </span>
    {/if}

    {#if evaluating}
      <span class="inline-flex items-center gap-1 rounded bg-warning-200 px-1.5 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        <span class="h-1.5 w-1.5 animate-pulse-subtle rounded-full bg-warning-500"></span>
        Evaluating
      </span>
    {/if}

    {#if topPR}
      <span class="inline-flex items-center gap-1 rounded bg-warning-500/15 px-1.5 py-0.5 font-medium text-warning-700 dark:text-warning-400" title={topPR.title}>
        <GitPullRequest size={12} />
        #{topPR.number}
        {#if topPR.reviewDecision === 'APPROVED'}
          <span class="text-success-500" title="Approved">✓</span>
        {:else if topPR.reviewDecision === 'CHANGES_REQUESTED'}
          <span class="text-error-500" title="Changes requested">✗</span>
        {/if}
        {#if topPR.mergeable === 'CONFLICTING'}
          <span class="text-error-500" title="Merge conflicts">⚠</span>
        {/if}
      </span>
    {:else if t.prNumber}
      <span class="inline-flex items-center gap-1 rounded bg-warning-500/15 px-1.5 py-0.5 font-medium text-warning-700 dark:text-warning-400">
        <GitPullRequest size={12} />
        #{t.prNumber}
      </span>
    {/if}

    {#if t.issue}
      <span
        class="inline-flex items-center gap-1 rounded bg-secondary-500/15 px-1.5 py-0.5 font-medium text-secondary-700 dark:text-secondary-400"
        title={t.issue}
      >
        <CircleDot size={12} />
        Issue
      </span>
    {/if}

    {#if t.tags?.length}
      {#each t.tags as tag}
        <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
          {tag}
        </span>
      {/each}
    {/if}

    <span class="ml-auto opacity-60">{timeAgo(t.updatedAt)}</span>
  </div>
  </button>
  {#if onstatuschange}
    <div class="mt-2 flex justify-end">
      <select
        value={t.status}
        onchange={handleStatusChange}
        onclick={(e) => e.stopPropagation()}
        class="tap rounded border border-surface-300 bg-surface-100 px-2 py-1 text-xs font-medium dark:border-surface-600 dark:bg-surface-700"
        aria-label="Change status"
      >
        {#each STATUS_OPTIONS as opt}
          <option value={opt.value} disabled={hasRunningAgent && AGENT_BLOCKED_STATUSES.has(opt.value)}>{opt.label}</option>
        {/each}
      </select>
    </div>
  {/if}
</div>
