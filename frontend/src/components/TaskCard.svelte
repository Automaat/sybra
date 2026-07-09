<script lang="ts">
  import { timeAgo } from '$lib/dates.js'
  import { CheckCircle, XCircle, Clock, GitPullRequest, GitPullRequestDraft, CircleDot, AlertTriangle, MoreHorizontal, Eye, PenLine, Hourglass, ShieldCheck, Loader, Wrench, MessageSquare, Hash } from '@lucide/svelte'
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { awaitsHumanLabel, coreStatus, statusLabel } from '../lib/statuses.js'
  import { isReviewTask as isReviewTaskFn, reviewPhaseMeta, type ReviewPhaseIcon } from '../lib/review-phase.js'
  import { isOwnPRTask as isOwnPRTaskFn, prPhaseMeta, type PRPhaseIcon } from '../lib/pr-phase.js'
  import { activeTaskNeedsUserAttention } from '../lib/task-attention.js'
  import { PRIORITY_OPTIONS } from '../lib/priorities.js'
  import { projectShortName, projectDotStyle } from '../lib/project-cue.js'
  import type { UmbrellaProgress } from '../lib/umbrella-progress.js'
  import Pill from './Pill.svelte'

  interface Props {
    task: Task
    umbrellaProgress?: UmbrellaProgress | null
    onclick: () => void
    focused?: boolean
  }

  const { task: t, umbrellaProgress = null, onclick, focused = false }: Props = $props()

  let dragging = $state(false)
  let actionsMenuOpen = $state(false)

  // O(1) lookup into the store's precomputed per-task status map instead of
  // scanning the full agent list 4× per render.
  const agentStatus = $derived(agentStore.agentStatusByTask.get(t.id))
  const triaging = $derived(agentStatus?.triaging ?? false)
  const evaluating = $derived(agentStatus?.evaluating ?? false)
  const planning = $derived(agentStatus?.planning ?? false)
  const agentRunning = $derived(agentStatus?.running ?? false)

  const linkedPRs = $derived(reviewStore.byTask(t))
  const topPR = $derived(linkedPRs.length > 0 ? linkedPRs[0] : null)
  const isReviewTask = $derived(isReviewTaskFn(t))
  // Own-PR task with a computed lifecycle phase (In Review column).
  const ownPRPhase = $derived(isOwnPRTaskFn(t))

  // lucide components keyed by the phase meta's icon name.
  const PHASE_ICONS: Record<ReviewPhaseIcon, typeof CheckCircle> = {
    loader: Loader, eye: Eye, pen: PenLine, hourglass: Hourglass, shield: ShieldCheck, check: CheckCircle, conflict: AlertTriangle,
  }

  // lucide components for the outbound PR phase glyph.
  const PR_PHASE_ICONS: Record<PRPhaseIcon, typeof CheckCircle> = {
    draft: GitPullRequestDraft, loader: Loader, wrench: Wrench, comment: MessageSquare, hourglass: Hourglass, check: CheckCircle,
  }

  // Task is waiting on the user (not an agent) — drives the red tile accent.
  // The strict own-PR phases (draft / approved) count too, so the card flags
  // "your move" while staying in the In Review column.
  const needsYou = $derived(activeTaskNeedsUserAttention(t))

  // A granular sub-state folded into this column that ISN'T an attention state
  // (e.g. `new` in Todo, `ready-review` in In Review). The column shows the
  // workflow stage; this quiet badge shows the precise sub-state, keeping the
  // two board axes separate instead of silently merging the state away.
  const subStateLabel = $derived(
    !needsYou && coreStatus(t.status) !== t.status ? statusLabel(t.status) : '',
  )

  function priorityMeta(p: string | undefined) {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? '')) ?? PRIORITY_OPTIONS[0]
  }

  const visibleTags = $derived((t.tags ?? []).filter((tag) => tag !== 'umbrella-gated'))

  // Prefer the live PR record's URL; fall back to constructing one from the
  // task's own project + PR number when the review store hasn't matched it.
  const prLink = $derived(
    topPR?.url || (t.prNumber && t.projectId ? `https://github.com/${t.projectId}/pull/${t.prNumber}` : ''),
  )

  async function copyToClipboard(value: string, label: string) {
    actionsMenuOpen = false
    try {
      await navigator.clipboard.writeText(value)
      notificationStore.pushLocal('success', 'Copied', label)
    } catch (e) {
      notificationStore.pushLocal('error', 'Copy failed', String(e))
    }
  }

  $effect(() => {
    if (!actionsMenuOpen) return
    function onKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        actionsMenuOpen = false
      }
    }
    window.addEventListener('keydown', onKeydown, { capture: true })
    return () => window.removeEventListener('keydown', onKeydown, { capture: true })
  })

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
    class="flex w-full cursor-grab flex-col items-stretch gap-1.5 text-left active:cursor-grabbing"
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

  {#snippet issueGlyph()}
    <!-- A linked issue alongside a PR: a real tooltip + accessible name (a
         lucide `title` prop only lands as an inert SVG attribute). -->
    <span title={t.issue} aria-label="Also linked to an issue" class="inline-flex shrink-0 items-center">
      <CircleDot size={11} class="opacity-60" aria-hidden="true" />
    </span>
  {/snippet}

  <div class="flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    {#if t.agentMode === 'interactive'}
      <span
        class="inline-flex items-center rounded bg-surface-200 px-1.5 py-0.5 text-surface-500 dark:bg-surface-700 dark:text-surface-400"
        title="Interactive agent"
      >
        interactive
      </span>
    {/if}

    {#if t.projectId}
      <Pill role="project" title={t.projectId}>
        <span class="h-2 w-2 shrink-0 rounded-full" style={projectDotStyle(t.projectId)}></span>
        {projectShortName(t.projectId)}
      </Pill>
    {/if}

    {#if t.taskType === 'umbrella' && umbrellaProgress && umbrellaProgress.total > 0}
      <Pill role="reference" title="{umbrellaProgress.done}/{umbrellaProgress.total} subissues complete">
        {umbrellaProgress.done}/{umbrellaProgress.total}
      </Pill>
    {/if}

    {#if triaging}
      <span class="inline-flex items-center gap-1 rounded bg-primary-200 px-1.5 py-0.5 text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        <span class="h-1.5 w-1.5 rounded-full bg-primary-500"></span>
        Triaging
      </span>
    {/if}

    {#if planning}
      <span class="inline-flex items-center gap-1 rounded bg-tertiary-200 px-1.5 py-0.5 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        <span class="h-1.5 w-1.5 rounded-full bg-tertiary-500"></span>
        Planning
      </span>
    {/if}

    {#if needsYou && !isReviewTask && !ownPRPhase}
      <Pill role="attention" class="bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200">
        {awaitsHumanLabel(t.status)}
      </Pill>
    {:else if t.tags?.includes('umbrella-gated') && !isReviewTask && !ownPRPhase}
      <span class="inline-flex items-center gap-1 rounded bg-surface-200 px-1.5 py-0.5 text-surface-600 dark:bg-surface-700 dark:text-surface-300">
        <Hourglass size={11} class="shrink-0" />
        Waiting
      </span>
    {:else if subStateLabel && !isReviewTask && !ownPRPhase}
      <span class="inline-flex items-center rounded-full bg-surface-200 px-2 py-0.5 text-surface-600 dark:bg-surface-700 dark:text-surface-300">
        {subStateLabel}
      </span>
    {/if}

    {#if t.status === 'testing'}
      <span
        class="inline-flex items-center gap-1 rounded bg-secondary-200 px-1.5 py-0.5 text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200"
        title={agentRunning ? 'Adversarial testing in progress' : 'Waiting for a testing slot'}
      >
        <span class="h-1.5 w-1.5 rounded-full bg-secondary-500 {agentRunning ? 'animate-pulse' : ''}"></span>
        {agentRunning ? 'Testing' : 'Queued'}
      </span>
    {:else if agentRunning}
      <span class="inline-flex items-center gap-1 rounded bg-success-200 px-1.5 py-0.5 text-success-800 dark:bg-success-700 dark:text-success-200">
        <span class="h-1.5 w-1.5 rounded-full bg-success-500"></span>
        Agent
      </span>
    {/if}

    {#if evaluating}
      <span class="inline-flex items-center gap-1 rounded bg-warning-200 px-1.5 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        <span class="h-1.5 w-1.5 rounded-full bg-warning-500"></span>
        Evaluating
      </span>
    {/if}

    {#if isReviewTask}
      {@const ph = reviewPhaseMeta(t)}
      {@const PhaseIcon = PHASE_ICONS[ph.icon]}
      <span
        class="inline-flex min-w-0 max-w-full items-center gap-1 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium {ph.classes}"
        title={t.statusReason || ph.label}
      >
        <PhaseIcon size={12} class="shrink-0" />
        {#if topPR}<span class="shrink-0">#{topPR.number}</span>{:else if t.prNumber}<span class="shrink-0">#{t.prNumber}</span>{/if}
        <span class="min-w-0 truncate">{ph.label}</span>
        {#if t.issue}{@render issueGlyph()}{/if}
      </span>
    {:else if ownPRPhase}
      {@const ph = prPhaseMeta(t)}
      {@const PhaseIcon = PR_PHASE_ICONS[ph.icon]}
      <span
        class="inline-flex min-w-0 max-w-full items-center gap-1 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium {ph.classes}"
        title={t.statusReason || ph.label}
      >
        <PhaseIcon size={12} class="shrink-0" />
        {#if topPR}<span class="shrink-0">#{topPR.number}</span>{:else if t.prNumber}<span class="shrink-0">#{t.prNumber}</span>{/if}
        <span class="min-w-0 truncate">{ph.label}</span>
        {#if t.issue}{@render issueGlyph()}{/if}
      </span>
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
        {#if t.issue}{@render issueGlyph()}{/if}
      </Pill>
    {:else if t.prNumber}
      <Pill role="reference">
        <GitPullRequest size={12} />
        #{t.prNumber}
        {#if t.issue}{@render issueGlyph()}{/if}
      </Pill>
    {:else if t.issue}
      <Pill role="reference" title={t.issue}>
        <CircleDot size={12} />
        Issue
      </Pill>
    {/if}

    {#if visibleTags.length}
      <Pill role="tag">{visibleTags[0]}</Pill>
      {#if visibleTags.length > 1}
        <span class="text-surface-400" title={visibleTags.join(', ')}>+{visibleTags.length - 1}</span>
      {/if}
    {/if}

    <span class="ml-auto text-[11px] text-surface-400/80">{timeAgo(t.updatedAt)}</span>
  </div>
  </button>
  {#if focused}
    <div class="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-surface-400">
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">Enter</kbd><span>open</span>
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">S</kbd><span>status</span>
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">P</kbd><span>priority</span>
      <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">⌘I</kbd><span>sidebar</span>
    </div>
  {/if}
  <div class="absolute right-1 top-1">
    <button
      type="button"
      onclick={(e) => { e.stopPropagation(); actionsMenuOpen = !actionsMenuOpen }}
      class="rounded p-1 text-surface-400 opacity-0 transition-opacity hover:bg-surface-200 hover:text-surface-600 focus:opacity-100 group-hover:opacity-100 dark:hover:bg-surface-600 dark:hover:text-surface-300 {actionsMenuOpen ? 'opacity-100' : ''}"
      aria-haspopup="menu"
      aria-expanded={actionsMenuOpen}
      aria-label="Task actions"
      title="Copy…"
    >
      <MoreHorizontal size={14} />
    </button>
    {#if actionsMenuOpen}
      <button
        type="button"
        tabindex="-1"
        class="fixed inset-0 z-40 cursor-default"
        aria-label="Close menu"
        onclick={(e) => { e.stopPropagation(); actionsMenuOpen = false }}
      ></button>
      <div role="menu" class="absolute right-0 z-50 mt-1 w-44 rounded-lg py-1 elevation-popover">
        <button
          type="button"
          role="menuitem"
          class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs hover:bg-surface-200 dark:hover:bg-surface-700"
          onclick={(e) => { e.stopPropagation(); copyToClipboard(t.id, 'Task ID copied') }}
        >
          <Hash size={12} class="shrink-0 text-surface-400" />
          Copy task ID
        </button>
        {#if t.issue}
          <button
            type="button"
            role="menuitem"
            class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs hover:bg-surface-200 dark:hover:bg-surface-700"
            onclick={(e) => { e.stopPropagation(); copyToClipboard(t.issue, 'Issue link copied') }}
          >
            <CircleDot size={12} class="shrink-0 text-surface-400" />
            Copy issue link
          </button>
        {/if}
        {#if prLink}
          <button
            type="button"
            role="menuitem"
            class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs hover:bg-surface-200 dark:hover:bg-surface-700"
            onclick={(e) => { e.stopPropagation(); copyToClipboard(prLink, 'PR link copied') }}
          >
            <GitPullRequest size={12} class="shrink-0 text-surface-400" />
            Copy PR link
          </button>
        {/if}
      </div>
    {/if}
  </div>
</div>
