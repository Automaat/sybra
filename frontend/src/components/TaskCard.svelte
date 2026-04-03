<script lang="ts">
  import type { task } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'

  interface Props {
    task: task.Task
    onclick: () => void
  }

  const { task: t, onclick }: Props = $props()

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

<button
  type="button"
  draggable="true"
  class="w-full rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700 {dragging ? 'opacity-40' : ''}"
  onclick={onclick}
  ondragstart={(e) => {
    dragging = true
    e.dataTransfer!.setData('text/plain', t.id)
    e.dataTransfer!.effectAllowed = 'move'
  }}
  ondragend={() => { dragging = false }}
>
  <div class="mb-1.5 flex items-center gap-1.5">
    {#if topPR?.ciStatus === 'SUCCESS'}
      <svg class="h-4 w-4 shrink-0 text-green-500" viewBox="0 0 16 16" fill="currentColor"><title>CI passed</title><path d="M8 16A8 8 0 1 1 8 0a8 8 0 0 1 0 16Zm3.78-9.72a.751.751 0 0 0-1.06-1.06L6.75 9.19 5.28 7.72a.751.751 0 0 0-1.06 1.06l2 2a.75.75 0 0 0 1.06 0l4.5-4.5Z"/></svg>
    {:else if topPR?.ciStatus === 'FAILURE'}
      <svg class="h-4 w-4 shrink-0 text-red-500" viewBox="0 0 16 16" fill="currentColor"><title>CI failed</title><path d="M2.343 13.657A8 8 0 1 1 13.658 2.343 8 8 0 0 1 2.343 13.657ZM6.03 4.97a.751.751 0 0 0-1.06 1.06L6.94 8 4.97 9.97a.751.751 0 1 0 1.06 1.06L8 9.06l1.97 1.97a.751.751 0 1 0 1.06-1.06L9.06 8l1.97-1.97a.751.751 0 1 0-1.06-1.06L8 6.94 6.03 4.97Z"/></svg>
    {:else if topPR?.ciStatus === 'PENDING'}
      <svg class="h-4 w-4 shrink-0 text-yellow-500" viewBox="0 0 16 16" fill="currentColor"><title>CI pending</title><path d="M8 16A8 8 0 1 1 8 0a8 8 0 0 1 0 16Zm1-8.577V4.75a.75.75 0 0 0-1.5 0V8a.75.75 0 0 0 .388.657l3 1.5a.75.75 0 1 0 .67-1.342L9 7.423Z"/></svg>
    {/if}
    <h3 class="text-sm font-semibold leading-tight">{t.title}</h3>
  </div>

  <div class="flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
      {t.agentMode}
    </span>

    {#if t.projectId}
      <span class="rounded bg-primary-100 px-1.5 py-0.5 text-primary-700 dark:bg-primary-800 dark:text-primary-300">
        {t.projectId}
      </span>
    {/if}

    {#if triaging}
      <span class="inline-flex items-center gap-1 rounded bg-primary-200 px-1.5 py-0.5 text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
        Triaging
      </span>
    {/if}

    {#if planning}
      <span class="inline-flex items-center gap-1 rounded bg-tertiary-200 px-1.5 py-0.5 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
        Planning
      </span>
    {/if}

    {#if agentRunning}
      <span class="inline-flex items-center gap-1 rounded bg-success-200 px-1.5 py-0.5 text-success-800 dark:bg-success-700 dark:text-success-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-success-500"></span>
        Agent
      </span>
    {/if}

    {#if evaluating}
      <span class="inline-flex items-center gap-1 rounded bg-warning-200 px-1.5 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
        Evaluating
      </span>
    {/if}

    {#if topPR}
      <span class="inline-flex items-center gap-1 rounded bg-purple-500/15 px-1.5 py-0.5 font-medium text-purple-600 dark:text-purple-400" title={topPR.title}>
        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor"><path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354Z"/></svg>
        #{topPR.number}
        {#if topPR.reviewDecision === 'APPROVED'}
          <span class="text-green-500" title="Approved">✓</span>
        {:else if topPR.reviewDecision === 'CHANGES_REQUESTED'}
          <span class="text-red-500" title="Changes requested">✗</span>
        {/if}
        {#if topPR.mergeable === 'CONFLICTING'}
          <span class="text-red-500" title="Merge conflicts">⚠</span>
        {/if}
      </span>
    {:else if t.prNumber}
      <span class="inline-flex items-center gap-1 rounded bg-purple-500/15 px-1.5 py-0.5 font-medium text-purple-600 dark:text-purple-400">
        <svg class="h-3 w-3" viewBox="0 0 16 16" fill="currentColor"><path d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354Z"/></svg>
        #{t.prNumber}
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
