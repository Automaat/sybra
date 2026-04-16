<script lang="ts">
  import { Loader } from '@lucide/svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { TimestampedStreamEvent } from '$lib/timeline.js'
  import { extractBashActivity, stripAnsi, truncateOutput } from '$lib/bash-activity.js'

  interface Props {
    streamOutputs: TimestampedStreamEvent[]
    convoEvents: agent.ConvoEvent[]
  }

  const { streamOutputs, convoEvents }: Props = $props()

  const activities = $derived(extractBashActivity(streamOutputs, convoEvents))
</script>

{#if activities.length === 0}
  <div class="flex items-center justify-center py-12 text-sm text-surface-400">
    No shell activity yet
  </div>
{:else}
  <div class="flex flex-col gap-3 p-3">
    {#each activities as act (act.id)}
      <div class="rounded-lg border border-surface-300 dark:border-surface-700 overflow-hidden">
        <!-- Command header -->
        <div
          class="flex items-center gap-2 px-3 py-2 font-mono text-xs
            {act.isError
              ? 'bg-error-50 dark:bg-error-950/40'
              : 'bg-surface-100 dark:bg-surface-800'}"
        >
          {#if act.status === 'running'}
            <Loader size={12} class="animate-spin shrink-0 text-primary-500" />
          {:else}
            <span
              class="h-2 w-2 shrink-0 rounded-full
                {act.isError ? 'bg-error-500' : 'bg-success-500'}"
            ></span>
          {/if}
          <span class="min-w-0 flex-1 truncate text-surface-700 dark:text-surface-300">
            {act.command}
          </span>
          {#if act.status === 'running'}
            <span class="shrink-0 text-surface-400">running…</span>
          {/if}
        </div>

        <!-- Output (collapsed when empty) -->
        {#if act.output}
          <details>
            <summary class="cursor-pointer select-none px-3 py-1 text-[10px] text-surface-400 hover:text-surface-600 dark:hover:text-surface-300">
              Output
            </summary>
            <pre class="overflow-x-auto whitespace-pre-wrap break-all px-3 py-2 font-mono text-[11px] text-surface-700 dark:text-surface-300 bg-surface-50 dark:bg-surface-900">{stripAnsi(truncateOutput(act.output))}</pre>
          </details>
        {/if}
      </div>
    {/each}
  </div>
{/if}
