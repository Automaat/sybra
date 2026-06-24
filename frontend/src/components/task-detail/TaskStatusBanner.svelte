<script lang="ts">
  import { AlertTriangle, Info } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { statusSummary } from '../../lib/status-summary.js'
  import { timeAgo } from '$lib/dates.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  const summary = $derived(statusSummary(task.status))
  const freshness = $derived(timeAgo(task.updatedAt))
  // Attention sub-states (or any status_reason) warrant the warm banner; a
  // quiet folded sub-state uses neutral surface styling.
  const tone = $derived(summary?.tone === 'info' && !task.statusReason ? 'info' : 'attention')
</script>

{#if summary || task.statusReason}
  <div
    role="status"
    class="flex items-start gap-2 rounded-md border px-3 py-2 text-sm {tone === 'attention'
      ? 'border-warning-300 bg-warning-50 text-warning-800 dark:border-warning-700 dark:bg-warning-900/40 dark:text-warning-200'
      : 'border-surface-300 bg-surface-100 text-surface-700 dark:border-surface-600 dark:bg-surface-800 dark:text-surface-300'}"
  >
    {#if tone === 'attention'}
      <AlertTriangle size={16} class="mt-0.5 shrink-0" />
    {:else}
      <Info size={16} class="mt-0.5 shrink-0" />
    {/if}
    <div class="flex flex-col gap-0.5">
      {#if summary}
        <span>
          <span class="font-semibold">{summary.label}</span>{#if summary.hint} — {summary.hint}{/if}{#if freshness}<span class="opacity-70"> · updated {freshness}</span>{/if}
        </span>
      {/if}
      {#if task.statusReason}
        <span class={summary ? 'opacity-90' : ''}>{task.statusReason}</span>
      {/if}
    </div>
  </div>
{/if}
