<script lang="ts">
  import type { github } from '../../wailsjs/go/models.js'
  import { BrowserOpenURL } from '../../wailsjs/runtime/runtime.js'

  interface Props {
    pr: github.PullRequest
    actionLabel?: string
    onaction?: () => void
  }

  const { pr, actionLabel, onaction }: Props = $props()

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
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="link"
  tabindex="0"
  class="w-full cursor-pointer rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
  onclick={() => BrowserOpenURL(pr.url)}
  onkeydown={(e) => { if (e.key === 'Enter') BrowserOpenURL(pr.url) }}
>
  <div class="flex items-start justify-between gap-2">
    <div class="flex items-center gap-2">
      {#if pr.ciStatus}
        <span
          class="inline-block h-2.5 w-2.5 shrink-0 rounded-full {pr.ciStatus === 'SUCCESS' ? 'bg-green-500' : pr.ciStatus === 'FAILURE' ? 'bg-red-500' : 'bg-yellow-500'}"
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
        <span class="rounded bg-green-500/15 px-1.5 py-0.5 text-xs font-medium text-green-600 dark:text-green-400">Approved</span>
      {:else if pr.reviewDecision === 'CHANGES_REQUESTED'}
        <span class="rounded bg-red-500/15 px-1.5 py-0.5 text-xs font-medium text-red-600 dark:text-red-400">Changes</span>
      {/if}
      {#if pr.unresolvedCount > 0}
        <span class="rounded bg-yellow-500/15 px-1.5 py-0.5 text-xs font-medium text-yellow-600 dark:text-yellow-400"
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
