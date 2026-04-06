<script lang="ts">
  import type { github } from '../../wailsjs/go/models.js'
  import { BrowserOpenURL } from '../../wailsjs/runtime/runtime.js'

  interface Props {
    issue: github.Issue
  }

  const { issue }: Props = $props()

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
  onclick={() => BrowserOpenURL(issue.url)}
  onkeydown={(e) => { if (e.key === 'Enter') BrowserOpenURL(issue.url) }}
>
  <div class="flex items-start justify-between gap-2">
    <h3 class="text-sm font-semibold leading-tight">{issue.title}</h3>
  </div>

  <div class="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs text-surface-500">
    <span class="font-mono">{issue.repository}#{issue.number}</span>
    <span>by {issue.author}</span>

    {#if issue.labels?.length}
      {#each issue.labels as label}
        <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{label}</span>
      {/each}
    {/if}

    <span class="ml-auto opacity-60">{timeAgo(issue.updatedAt)}</span>
  </div>
</div>
