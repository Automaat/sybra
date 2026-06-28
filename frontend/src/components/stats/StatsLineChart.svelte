<script lang="ts">
  import type { TimeSeriesPoint } from '../../lib/stats-charts.js'

  interface Props {
    points: TimeSeriesPoint[]
    ariaLabel: string
    emptyLabel: string
  }

  const { points, ariaLabel, emptyLabel }: Props = $props()

  const W = 320
  const H = 96
  const PAD = 6

  const max = $derived(Math.max(0, ...points.map((p) => p.value)))

  // x evenly spaced; y inverted (SVG origin top-left), scaled to max.
  function px(i: number): number {
    if (points.length <= 1) return W / 2
    return PAD + (i / (points.length - 1)) * (W - 2 * PAD)
  }
  function py(value: number): number {
    if (max <= 0) return H - PAD
    return H - PAD - (value / max) * (H - 2 * PAD)
  }

  const line = $derived(points.map((p, i) => `${px(i)},${py(p.value)}`).join(' '))
  const area = $derived(
    points.length > 0 ? `${px(0)},${H - PAD} ${line} ${px(points.length - 1)},${H - PAD}` : '',
  )
</script>

{#if points.length === 0}
  <p class="py-8 text-center text-xs text-surface-400">{emptyLabel}</p>
{:else}
  <svg viewBox="0 0 {W} {H}" class="h-24 w-full" preserveAspectRatio="none" role="img" aria-label={ariaLabel}>
    {#if points.length > 1}
      <polygon points={area} class="fill-primary-500/10" />
      <polyline points={line} fill="none" class="stroke-primary-500" stroke-width="2" vector-effect="non-scaling-stroke" />
    {/if}
    {#each points as p, i (p.date)}
      <circle cx={px(i)} cy={py(p.value)} r="2.5" class="fill-primary-500" vector-effect="non-scaling-stroke" />
    {/each}
  </svg>
  <div class="mt-1 flex justify-between text-[10px] text-surface-400">
    <span>{points[0].date}</span>
    {#if points.length > 1}
      <span>{points[points.length - 1].date}</span>
    {/if}
  </div>
{/if}
