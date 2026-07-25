<script lang="ts">
  import StatsLineChart from './stats/StatsLineChart.svelte'
  import { pct } from '$lib/evaluation-format.js'
  import type { AutonomySnapshot, AutonomyTrend } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'
  import type { TimeSeriesPoint } from '$lib/stats-charts.js'

  interface Props {
    trend: AutonomyTrend | null
  }

  const { trend }: Props = $props()

  // A zero-value AutonomyTrend (service disabled / not yet loaded) still has a
  // non-null overall, which would render as a measured 0% dashboard — same
  // guard the headline scorecard applies to its own hasData check.
  const hasData = $derived(!!trend && trend.overall.tasksLanded > 0)

  const tiles = $derived(
    trend
      ? [
          { label: 'Overall', snapshot: trend.overall },
          { label: 'Last 30 days', snapshot: trend.lastMonth },
          { label: 'Last 7 days', snapshot: trend.lastWeek },
        ]
      : [],
  )

  const weeklyPoints = $derived<TimeSeriesPoint[]>(
    (trend?.weekly ?? []).map((w) => ({ date: String(w.weekStart).slice(0, 10), value: w.autonomyRate * 100 })),
  )

  // Higher is better; same thresholds as the headline scorecard's goodScale.
  function goodScale(x: number): string {
    if (x >= 0.8) return 'text-success-600 dark:text-success-400'
    if (x >= 0.5) return 'text-warning-600 dark:text-warning-400'
    return 'text-error-600 dark:text-error-400'
  }

  function detail(s: AutonomySnapshot): string {
    return `${s.autonomousLandings}/${s.tasksLanded} landed`
  }
</script>

{#if hasData}
  <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
    <h3 class="mb-3 text-sm font-semibold text-surface-500">Autonomy over time</h3>
    <div class="mb-4 grid grid-cols-1 gap-4 sm:grid-cols-3">
      {#each tiles as tile (tile.label)}
        <div>
          <span class="text-xs font-medium text-surface-500">{tile.label}</span>
          <p class="mt-1 text-2xl font-bold {goodScale(tile.snapshot.autonomyRate)}">{pct(tile.snapshot.autonomyRate)}</p>
          <p class="mt-0.5 text-xs text-surface-400">{detail(tile.snapshot)}</p>
        </div>
      {/each}
    </div>
    <StatsLineChart points={weeklyPoints} ariaLabel="Autonomy rate by week" emptyLabel="Not enough weekly data yet" />
  </div>
{/if}
