<script lang="ts">
  import { fly } from 'svelte/transition'
  import type { task } from '../../../wailsjs/go/models.js'

  interface Props {
    linkedTask?: task.Task | null
  }

  const { linkedTask }: Props = $props()
</script>

<div class="flex flex-col gap-6" in:fly={{ y: 8, duration: 150 }}>
  <div class="rounded-xl border border-surface-300 bg-surface-50 p-6 dark:border-surface-700 dark:bg-surface-900">
    <div class="mb-4 flex items-center gap-3">
      <span class="flex items-center gap-1.5 rounded-full bg-surface-200 px-3 py-1 text-sm font-medium text-surface-600 dark:bg-surface-700 dark:text-surface-300">
        <span class="h-2 w-2 animate-pulse rounded-full bg-surface-400"></span>
        Agent starting…
      </span>
    </div>

    {#if linkedTask}
      <div class="prose prose-sm max-w-none dark:prose-invert">
        <h2 class="mb-2 text-lg font-semibold">{linkedTask.title}</h2>
        {#if linkedTask.body}
          <p class="whitespace-pre-wrap text-sm text-surface-700 dark:text-surface-300">{linkedTask.body}</p>
        {:else}
          <p class="text-sm text-surface-400">No description provided.</p>
        {/if}
      </div>
    {:else}
      <p class="text-sm text-surface-400">Agent hasn't started producing output yet.</p>
    {/if}
  </div>
</div>
