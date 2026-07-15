<script lang="ts">
  import { timeAgo } from '$lib/dates.js'
  import type { PullRequest } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

  interface Props {
    pr: PullRequest
    actionLabel?: string
    onaction?: () => void
    onselect?: () => void
  }

  const { pr, actionLabel, onaction, onselect }: Props = $props()

</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="link"
  tabindex="0"
  class="w-full cursor-pointer rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
  onclick={() => onselect?.()}
  onkeydown={(e) => { if (e.key === 'Enter') onselect?.() }}
>
  <div class="flex items-start justify-between gap-2">
    <div class="flex items-center gap-2">
      {#if pr.ciStatus}
        <span
          class="inline-block h-2.5 w-2.5 shrink-0 rounded-full {pr.ciStatus === 'SUCCESS' ? 'bg-success-500' : pr.ciStatus === 'FAILURE' ? 'bg-error-500' : 'bg-warning-500'}"
          title="CI: {pr.ciStatus.toLowerCase()}"
        ></span>
      {/if}
      <h3 class="text-sm font-semibold leading-tight">{pr.title}</h3>
    </div>
    <div class="flex shrink-0 items-center gap-1.5">
      {#if pr.isDraft}
        <span class="rounded bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">Draft</span>
      {/if}
      {#if pr.reviewDecision === 'APPROVED'}
        <span class="rounded bg-success-500/15 px-1.5 py-0.5 text-xs font-medium text-success-700 dark:text-success-400">Approved</span>
      {:else if pr.reviewDecision === 'CHANGES_REQUESTED'}
        <span class="rounded bg-error-500/15 px-1.5 py-0.5 text-xs font-medium text-error-700 dark:text-error-400">Changes</span>
      {/if}
      {#if pr.unresolvedCount > 0}
        <span class="rounded bg-warning-500/15 px-1.5 py-0.5 text-xs font-medium text-warning-700 dark:text-warning-400"
          title="{pr.unresolvedCount} unresolved thread{pr.unresolvedCount !== 1 ? 's' : ''}"
        >{pr.unresolvedCount} unresolved</span>
      {/if}
    </div>
  </div>

  <div class="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="font-mono">{pr.repository}#{pr.number}</span>
    <span>by {pr.author}</span>

    {#if pr.labels?.length}
      {#each pr.labels as label}
        <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{label}</span>
      {/each}
    {/if}

    <span class="ml-auto opacity-60">{timeAgo(pr.updatedAt)}</span>
  </div>

  {#if onaction}
    <button
      type="button"
      class="mt-2 rounded bg-primary-600 px-2.5 py-1 text-xs font-medium text-white transition-colors hover:bg-primary-700"
      onclick={(e) => { e.stopPropagation(); onaction(); }}
    >
      {actionLabel ?? 'Action'}
    </button>
  {/if}
</div>
