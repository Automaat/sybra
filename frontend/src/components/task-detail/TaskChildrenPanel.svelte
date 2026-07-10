<script lang="ts">
  import { CheckCircle2, CircleDot, GitPullRequest } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { childrenForUmbrella, isChildComplete } from '../../lib/umbrella-progress.js'
  import { openLink } from '$lib/browser.svelte.js'
  import StatusBadge from '../StatusBadge.svelte'

  interface Props {
    task: Task
    onselecttask: (id: string) => void
  }

  const { task, onselecttask }: Props = $props()

  const data = $derived(childrenForUmbrella(task, taskStore.list))
  const completeCount = $derived(data.children.filter(isChildComplete).length)

  // Unresolved refs are normalized ("owner/repo#123"); reconstruct a GitHub
  // issue URL when the shape matches, otherwise fall back to plain text.
  function unresolvedUrl(ref: string): string | null {
    const m = ref.match(/^([^/]+)\/([^#]+)#(\d+)$/)
    if (!m) return null
    return `https://github.com/${m[1]}/${m[2]}/issues/${m[3]}`
  }
</script>

<div class="flex flex-col gap-3">
  {#if data.children.length > 0}
    <div class="flex items-center gap-2 text-sm text-surface-500">
      <span>{completeCount}/{data.children.length} local merged-outcome progress</span>
    </div>
  {/if}

  {#if data.children.length === 0 && data.unresolved.length === 0}
    <p class="text-sm opacity-60">No child tasks linked yet.</p>
  {:else}
    <ul class="flex flex-col gap-2">
      {#each data.children as child (child.id)}
        <li class="flex items-center gap-2 rounded-lg border border-surface-300 bg-surface-50 p-2.5 dark:border-surface-600 dark:bg-surface-800">
          <button
            type="button"
            class="flex min-w-0 flex-1 items-center gap-2 text-left"
            onclick={() => onselecttask(child.id)}
          >
            {#if isChildComplete(child)}
              <CheckCircle2 size={14} class="shrink-0 text-success-500" aria-label="Shipped (merged)" />
            {/if}
            <span class="min-w-0 flex-1 truncate text-sm font-medium">{child.title}</span>
            <StatusBadge status={child.status} />
          </button>
          {#if child.issue}
            <button
              type="button"
              class="shrink-0 text-surface-400 hover:text-surface-700 dark:hover:text-surface-200"
              title="Open issue on GitHub"
              onclick={(e) => { e.stopPropagation(); openLink(child.issue, e) }}
            >
              <CircleDot size={14} />
            </button>
          {/if}
          {#if child.prNumber && child.projectId}
            <button
              type="button"
              class="shrink-0 text-warning-600 hover:text-warning-800 dark:text-warning-400 dark:hover:text-warning-200"
              title="Open PR #{child.prNumber} on GitHub"
              onclick={(e) => { e.stopPropagation(); openLink(`https://github.com/${child.projectId}/pull/${child.prNumber}`, e) }}
            >
              <GitPullRequest size={14} />
            </button>
          {/if}
        </li>
      {/each}
      {#each data.unresolved as ref (ref)}
        {@const url = unresolvedUrl(ref)}
        <li class="flex items-center gap-2 rounded-lg border border-dashed border-surface-300 bg-surface-100/50 p-2.5 text-surface-500 dark:border-surface-600 dark:bg-surface-800/50">
          <span class="rounded bg-surface-200 px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wide dark:bg-surface-700">
            Not yet tracked
          </span>
          {#if url}
            <button
              type="button"
              class="min-w-0 flex-1 truncate text-left text-sm hover:underline"
              onclick={(e) => openLink(url, e)}
            >
              {ref}
            </button>
          {:else}
            <span class="min-w-0 flex-1 truncate text-sm">{ref}</span>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</div>
