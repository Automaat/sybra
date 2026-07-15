<script lang="ts">
  import { fly } from 'svelte/transition'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import type { AgentQueueSnapshotItem } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

  interface Props {
    linkedTask?: Task | null
    queueInfo?: AgentQueueSnapshotItem | null
  }

  const { linkedTask, queueInfo }: Props = $props()
</script>

<div class="flex flex-col gap-6" in:fly={{ y: 8, duration: 150 }}>
  <div class="rounded-xl border border-surface-300 bg-surface-50 p-6 dark:border-surface-700 dark:bg-surface-900">
    <div class="mb-4 flex items-center gap-3">
      <span class="flex items-center gap-1.5 rounded-full bg-surface-200 px-3 py-1 text-sm font-medium text-surface-600 dark:bg-surface-700 dark:text-surface-300">
        <span class="h-2 w-2 animate-pulse rounded-full bg-surface-400"></span>
        Waiting for a slot
      </span>
    </div>

    <div class="mb-4 flex flex-wrap items-center gap-3 text-sm text-surface-500 dark:text-surface-400">
      {#if queueInfo}
        <span class="rounded bg-surface-200 px-2.5 py-1 font-medium dark:bg-surface-700">
          Queue {queueInfo.position} of {queueInfo.depth}
        </span>
      {/if}
      <span>Agent is queued and will start when a worker slot frees up.</span>
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
      <p class="text-sm text-surface-400">Agent is waiting for a slot before it starts producing output.</p>
    {/if}
  </div>
</div>
