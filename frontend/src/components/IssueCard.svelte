<script lang="ts">
  import { timeAgo } from '$lib/dates.js'
  import type { Issue } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'
  import { openLink } from '$lib/browser.svelte.js'

  interface Props {
    issue: Issue
  }

  const { issue }: Props = $props()

</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="link"
  tabindex="0"
  class="w-full cursor-pointer rounded-lg border border-surface-300 bg-surface-50 p-3 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
  onclick={(e) => openLink(issue.url, e)}
  onkeydown={(e) => { if (e.key === 'Enter') openLink(issue.url, e) }}
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
