<script lang="ts">
  import { timeAgo } from '$lib/dates.js'
  import { CheckCircle, XCircle, Clock, GitPullRequest, CircleDot, Copy, AlertTriangle, MoreHorizontal } from '@lucide/svelte'
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { awaitsHuman, awaitsHumanLabel } from '../lib/statuses.js'
  import { PRIORITY_OPTIONS } from '../lib/priorities.js'
  import { projectShortName, projectDotStyle } from '../lib/project-cue.js'
  import StatusPicker from './StatusPicker.svelte'
  import Pill from './Pill.svelte'

  interface Props {
    task: Task
    onclick: () => void
    focused?: boolean
    onstatuschange?: (status: string) => void
  }

  const { task: t, onclick, focused = false, onstatuschange }: Props = $props()

  let dragging = $state(false)
  let copiedBranch = $state(false)
  let moveMenuOpen = $state(false)

  const taskBranchName = $derived(
    'sybra/' + (t.slug ? t.slug + '-' + t.id : t.id)
  )

  async function copyBranch(e: MouseEvent) {
    e.stopPropagation()
    try {
      await navigator.clipboard.writeText(taskBranchName)
      copiedBranch = true
      setTimeout(() => { copiedBranch = false }, 1500)
    } catch {
      notificationStore.pushLocal('error', 'Copy failed', 'Could not copy branch name to clipboard')
    }
  }

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
  const isReviewTask = $derived(t.tags?.includes('review') ?? false)

  // Task is waiting on the user (not an agent) — drives the red tile accent.
  const needsYou = $derived(awaitsHuman(t.status))

  function priorityMeta(p: string | undefined) {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? '')) ?? PRIORITY_OPTIONS[0]
  }

</script>

<div
  data-focused-task={focused ? '' : undefined}
  class="group relative w-full select-none rounded-lg border bg-surface-50 p-3 text-left transition-all duration-100 active:bg-surface-100 dark:bg-surface-800 dark:active:bg-surface-700 md:hover:bg-surface-100 md:dark:hover:bg-surface-700 {focused ? 'border-primary-400 ring-2 ring-primary-400/50 dark:border-primary-500 dark:ring-primary-500/50' : 'border-surface-300 dark:border-surface-600'} {needsYou ? 'border-l-4 border-l-error-500 dark:border-l-error-400' : ''} {needsYou && !focused ? 'ring-2 ring-error-400/40 dark:ring-error-500/40' : ''} {dragging ? 'opacity-40 shadow-lg' : ''}"
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
    {#if t.priority}
      {@const pm = priorityMeta(t.priority)}
      <span class="shrink-0 font-mono text-xs {pm.classes}" title="Priority: {pm.label}">{pm.icon}</span>
    {/if}
    <h3 class="line-clamp-2 text-sm font-semibold leading-tight">{t.title}</h3>
  </div>

  {#if t.statusReason}
    <p
      class="mb-1.5 flex items-start gap-1 line-clamp-2 text-xs text-warning-700 dark:text-warning-300"
      title={t.statusReason}
    >
      <AlertTriangle size={12} class="mt-0.5 shrink-0" />
      <span class="line-clamp-2">{t.statusReason}</span>
    </p>
  {/if}

  {#if t.blockedByIssue}
    <a
      href={t.blockedByIssue}
      target="_blank"
      rel="noopener"
      onclick={(e) => e.stopPropagation()}
      class="mb-1.5 inline-flex items-center gap-1 text-xs text-error-700 hover:underline dark:text-error-300"
    >
      <svg class="h-3 w-3 shrink-0" viewBox="0 0 16 16" fill="currentColor"><title>Issue</title><path d="M8 1.5a6.5 6.5 0 1 1 0 13 6.5 6.5 0 0 1 0-13Zm0 1a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11Zm0 2a.75.75 0 0 1 .75.75v3a.75.75 0 0 1-1.5 0v-3A.75.75 0 0 1 8 4.5Zm0 6a1 1 0 1 1 0 2 1 1 0 0 1 0-2Z"/></svg>
      Blocked by Sybra bug
    </a>
  {/if}

  <div class="flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">
      {t.agentMode}
    </span>

    {#if t.projectId}
      <Pill role="project" title={t.projectId}>
        <span class="h-2 w-2 shrink-0 rounded-full" style={projectDotStyle(t.projectId)}></span>
        {projectShortName(t.projectId)}
      </Pill>
    {/if}

    {#if t.branch}
      <span class="inline-flex items-center gap-1 rounded bg-surface-200 px-1.5 py-0.5 font-mono dark:bg-surface-700">
        <svg class="h-3 w-3 shrink-0" viewBox="0 0 16 16" fill="currentColor"><title>Branch</title><path d="M9.5 3.25a2.25 2.25 0 1 1 3 2.122V6A2.5 2.5 0 0 1 10 8.5H6a1 1 0 0 0-1 1v1.128a2.251 2.251 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.5 0v1.836A2.493 2.493 0 0 1 6 7h4a1 1 0 0 0 1-1v-.628A2.25 2.25 0 0 1 9.5 3.25Z"/></svg>
        {t.branch.replace(/^sybra\//, '')}
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

    {#if needsYou}
      <Pill role="attention" class="bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200">
        {awaitsHumanLabel(t.status)}
      </Pill>
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

    {#if topPR && isReviewTask}
      {#if topPR.viewerHasApproved}
        <Pill role="reference" title="Approved; waiting for PR to merge">
          <GitPullRequest size={12} />
          #{topPR.number}
          <span class="text-success-500" title="Approved">✓</span>
        </Pill>
      {:else}
        <Pill role="reference" title="Review requested">
          <GitPullRequest size={12} />
          #{topPR.number} Review
        </Pill>
      {/if}
    {:else if topPR}
      <Pill role="reference" title={topPR.title}>
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
      </Pill>
    {:else if t.prNumber}
      <Pill role="reference">
        <GitPullRequest size={12} />
        #{t.prNumber}
      </Pill>
    {/if}

    {#if t.issue}
      <Pill role="reference" title={t.issue}>
        <CircleDot size={12} />
        Issue
      </Pill>
    {/if}

    {#if t.tags?.length}
      {#each t.tags as tag}
        <Pill role="tag">{tag}</Pill>
      {/each}
    {/if}

    <span class="ml-auto opacity-60">{timeAgo(t.updatedAt)}</span>
  </div>
  </button>
  {#if t.projectId}
    <div class="mt-1.5 flex items-center opacity-0 transition-opacity group-hover:opacity-100">
      <button
        type="button"
        onclick={copyBranch}
        class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 font-mono text-xs text-surface-400 transition-colors hover:bg-surface-200 hover:text-surface-600 dark:hover:bg-surface-600 dark:hover:text-surface-300"
        title="Copy branch name (⇧⌘.)"
      >
        <Copy size={10} />
        {copiedBranch ? 'Copied!' : taskBranchName.replace(/^sybra\//, '')}
      </button>
    </div>
  {/if}
  {#if focused}
    <div class="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-surface-400">
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">Enter</kbd><span>open</span>
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">S</kbd><span>status</span>
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">P</kbd><span>priority</span>
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">⌘I</kbd><span>sidebar</span>
    </div>
  {/if}
  {#if onstatuschange}
    <button
      type="button"
      onclick={(e) => { e.stopPropagation(); moveMenuOpen = true }}
      class="absolute right-1 top-1 rounded p-1 text-surface-400 opacity-0 transition-opacity hover:bg-surface-200 hover:text-surface-600 focus:opacity-100 group-hover:opacity-100 dark:hover:bg-surface-600 dark:hover:text-surface-300"
      aria-label="Move task"
      title="Move to…"
    >
      <MoreHorizontal size={14} />
    </button>
    {#if moveMenuOpen}
      <StatusPicker
        currentStatus={t.status}
        onpick={(s) => { onstatuschange?.(s); moveMenuOpen = false }}
        onclose={() => { moveMenuOpen = false }}
      />
    {/if}
  {/if}
</div>
