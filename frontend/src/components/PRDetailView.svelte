<script lang="ts">
  import type { github } from '../../wailsjs/go/models.js'
  import { BrowserOpenURL } from '../../wailsjs/runtime/runtime.js'

  interface Props {
    pr: github.PullRequest
    checkRuns?: github.CheckRunInfo[]
    onback: () => void
    onapprove?: () => void
    onmerge?: () => void
    onrerun?: () => void
    onfix?: () => void
  }

  const { pr, checkRuns, onback, onapprove, onmerge, onrerun, onfix }: Props = $props()

  function timeAgo(date: string): string {
    if (!date) return ''
    const now = Date.now()
    const then = new Date(date).getTime()
    const diff = Math.floor((now - then) / 1000)
    if (diff < 60) return 'just now'
    if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
    if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
    return `${Math.floor(diff / 86400)}d ago`
  }

  const isEligible = $derived(
    !pr.isDraft &&
    pr.mergeable === 'MERGEABLE' &&
    (pr.ciStatus === 'SUCCESS' || pr.ciStatus === '')
  )

  const hasFailed = $derived(pr.ciStatus === 'FAILURE')
</script>

<div class="flex flex-col gap-4 p-6">
  <div class="flex items-center gap-3">
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-2.5 py-1 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={onback}
    >
      &larr; Back
    </button>
    <h2 class="text-lg font-semibold">{pr.title}</h2>
  </div>

  <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
    <div class="flex flex-wrap items-center gap-3 text-sm">
      <span class="font-mono text-surface-500">{pr.repository}#{pr.number}</span>
      <span class="text-surface-500">by {pr.author}</span>
      <span class="text-surface-400">{timeAgo(pr.createdAt)}</span>

      {#if pr.isDraft}
        <span class="rounded bg-surface-200 px-2 py-0.5 text-xs dark:bg-surface-700">Draft</span>
      {/if}
    </div>

    <div class="mt-4 flex flex-wrap items-center gap-2">
      {#if pr.ciStatus}
        <span class="inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium
          {pr.ciStatus === 'SUCCESS' ? 'bg-green-500/15 text-green-600 dark:text-green-400' :
           pr.ciStatus === 'FAILURE' ? 'bg-red-500/15 text-red-600 dark:text-red-400' :
           'bg-yellow-500/15 text-yellow-600 dark:text-yellow-400'}">
          <span class="inline-block h-2 w-2 rounded-full
            {pr.ciStatus === 'SUCCESS' ? 'bg-green-500' :
             pr.ciStatus === 'FAILURE' ? 'bg-red-500' : 'bg-yellow-500'}"></span>
          CI: {pr.ciStatus.toLowerCase()}
        </span>
      {/if}

      {#if pr.mergeable}
        <span class="rounded-full px-2.5 py-1 text-xs font-medium
          {pr.mergeable === 'MERGEABLE' ? 'bg-green-500/15 text-green-600 dark:text-green-400' :
           pr.mergeable === 'CONFLICTING' ? 'bg-red-500/15 text-red-600 dark:text-red-400' :
           'bg-surface-200 text-surface-500 dark:bg-surface-700'}">
          {pr.mergeable === 'MERGEABLE' ? 'Mergeable' :
           pr.mergeable === 'CONFLICTING' ? 'Conflicts' : 'Unknown'}
        </span>
      {/if}

      {#if pr.reviewDecision === 'APPROVED'}
        <span class="rounded-full bg-green-500/15 px-2.5 py-1 text-xs font-medium text-green-600 dark:text-green-400">Approved</span>
      {:else if pr.reviewDecision === 'CHANGES_REQUESTED'}
        <span class="rounded-full bg-red-500/15 px-2.5 py-1 text-xs font-medium text-red-600 dark:text-red-400">Changes Requested</span>
      {:else if pr.reviewDecision === 'REVIEW_REQUIRED'}
        <span class="rounded-full bg-yellow-500/15 px-2.5 py-1 text-xs font-medium text-yellow-600 dark:text-yellow-400">Review Required</span>
      {/if}

      {#if pr.unresolvedCount > 0}
        <span class="rounded-full bg-yellow-500/15 px-2.5 py-1 text-xs font-medium text-yellow-600 dark:text-yellow-400">
          {pr.unresolvedCount} unresolved
        </span>
      {/if}
    </div>

    {#if pr.labels?.length}
      <div class="mt-3 flex flex-wrap gap-1.5">
        {#each pr.labels as label}
          <span class="rounded bg-surface-200 px-2 py-0.5 text-xs dark:bg-surface-700">{label}</span>
        {/each}
      </div>
    {/if}
  </div>

  {#if checkRuns && checkRuns.length > 0}
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-5 dark:border-surface-600 dark:bg-surface-800">
      <h3 class="mb-3 text-sm font-semibold text-surface-500 uppercase tracking-wide">Check Runs</h3>
      <div class="flex flex-col gap-1.5">
        {#each checkRuns as check}
          <div class="flex items-center gap-2 text-sm">
            <span class="inline-block h-2 w-2 shrink-0 rounded-full
              {check.conclusion === 'SUCCESS' ? 'bg-green-500' :
               check.conclusion === 'FAILURE' ? 'bg-red-500' :
               check.status === 'IN_PROGRESS' ? 'bg-yellow-500' : 'bg-surface-400'}"></span>
            <span class="flex-1 truncate">{check.name}</span>
            <span class="text-xs text-surface-400">
              {check.conclusion ? check.conclusion.toLowerCase() : check.status?.toLowerCase() ?? ''}
            </span>
          </div>
        {/each}
      </div>
    </div>
  {/if}

  <div class="flex flex-wrap gap-2">
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={() => BrowserOpenURL(pr.url)}
    >
      Open in Browser
    </button>

    {#if onapprove && pr.reviewDecision !== 'APPROVED'}
      <button
        type="button"
        class="rounded-lg bg-green-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-green-700"
        onclick={onapprove}
      >
        Approve
      </button>
    {/if}

    {#if onmerge && isEligible}
      <button
        type="button"
        class="rounded-lg bg-primary-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-700"
        onclick={onmerge}
      >
        Merge
      </button>
    {/if}

    {#if onrerun && hasFailed}
      <button
        type="button"
        class="rounded-lg bg-yellow-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-yellow-700"
        onclick={onrerun}
      >
        Rerun Failed
      </button>
    {/if}

    {#if onfix && hasFailed}
      <button
        type="button"
        class="rounded-lg bg-red-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-red-700"
        onclick={onfix}
      >
        Fix
      </button>
    {/if}
  </div>
</div>
