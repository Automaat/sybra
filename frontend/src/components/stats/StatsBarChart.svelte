<script lang="ts">
  import type { ProjectCost } from '../../lib/stats-charts.js'

  interface Props {
    bars: ProjectCost[]
  }

  const { bars }: Props = $props()

  const max = $derived(Math.max(0, ...bars.map((b) => b.cost)))

  function width(cost: number): string {
    if (max <= 0) return '0%'
    return `${Math.max(2, (cost / max) * 100)}%`
  }
</script>

{#if bars.length === 0}
  <p class="py-8 text-center text-xs text-surface-400">No cost in this range</p>
{:else}
  <div class="flex flex-col gap-1.5">
    {#each bars as b (b.project)}
      <div class="flex items-center gap-2 text-xs">
        <span class="w-28 shrink-0 truncate font-mono text-surface-500" title={b.project}>{b.project}</span>
        <div class="h-3 flex-1 overflow-hidden rounded bg-surface-200 dark:bg-surface-700">
          <div class="h-full rounded bg-primary-500" style="width: {width(b.cost)}"></div>
        </div>
        <span class="w-14 shrink-0 text-right tabular-nums text-surface-600 dark:text-surface-300">${b.cost.toFixed(2)}</span>
      </div>
    {/each}
  </div>
{/if}
