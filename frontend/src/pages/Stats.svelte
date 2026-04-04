<script lang="ts">
  import { statsStore } from '../stores/stats.svelte.js'
  import type { stats } from '../../wailsjs/go/models.js'

  type Period = 'today' | 'thisWeek' | 'thisMonth' | 'allTime'

  let period = $state<Period>('allTime')

  const periods: { key: Period; label: string }[] = [
    { key: 'today', label: 'Today' },
    { key: 'thisWeek', label: 'This Week' },
    { key: 'thisMonth', label: 'This Month' },
    { key: 'allTime', label: 'All Time' },
  ]

  const summary = $derived(
    statsStore.data ? statsStore.data[period] : null,
  )

  $effect(() => {
    statsStore.load()
  })

  function formatDuration(seconds: number): string {
    if (seconds < 60) return `${seconds.toFixed(0)}s`
    if (seconds < 3600) return `${(seconds / 60).toFixed(1)}m`
    return `${(seconds / 3600).toFixed(1)}h`
  }

  function formatTokens(n: number): string {
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
    if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`
    return String(n)
  }

  function formatDate(ts: any): string {
    if (!ts) return ''
    const d = new Date(ts)
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) +
      ' ' + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  }

  function roleBadgeClasses(role: string): string {
    switch (role) {
      case 'triage': return 'bg-secondary-200 text-secondary-800 dark:bg-secondary-800 dark:text-secondary-200'
      case 'plan': return 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-800 dark:text-tertiary-200'
      case 'eval': return 'bg-warning-200 text-warning-800 dark:bg-warning-800 dark:text-warning-200'
      case 'review': return 'bg-primary-200 text-primary-800 dark:bg-primary-800 dark:text-primary-200'
      default: return 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'
    }
  }
</script>

<div class="flex flex-col gap-6 p-6">
  <div class="flex items-center justify-between">
    <h1 class="text-2xl font-bold">Stats</h1>
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={() => statsStore.load()}
    >
      Refresh
    </button>
  </div>

  {#if statsStore.error}
    <p class="text-error-500">{statsStore.error}</p>
  {/if}

  <!-- Period tabs -->
  <div class="flex gap-1 rounded-lg bg-surface-100 p-1 dark:bg-surface-800">
    {#each periods as p (p.key)}
      <button
        type="button"
        class="rounded-md px-4 py-1.5 text-sm font-medium transition-colors {period === p.key
          ? 'bg-white shadow dark:bg-surface-600 dark:text-white'
          : 'text-surface-500 hover:text-surface-700 dark:hover:text-surface-300'}"
        onclick={() => (period = p.key)}
      >
        {p.label}
      </button>
    {/each}
  </div>

  <!-- Summary cards -->
  {#if summary}
    <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5">
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Total Cost</span>
        <p class="mt-1 text-2xl font-bold">${summary.totalCostUsd.toFixed(2)}</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Total Runs</span>
        <p class="mt-1 text-2xl font-bold">{summary.totalRuns}</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Avg Cost / Run</span>
        <p class="mt-1 text-2xl font-bold">${summary.avgCostPerRun.toFixed(4)}</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Total Duration</span>
        <p class="mt-1 text-2xl font-bold">{formatDuration(summary.totalDurationS)}</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Tokens (In / Out)</span>
        <p class="mt-1 text-2xl font-bold">
          {formatTokens(summary.totalInputTokens)} / {formatTokens(summary.totalOutputTokens)}
        </p>
      </div>
    </div>
  {/if}

  <!-- Breakdowns -->
  {#if statsStore.data}
    <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
      {#each [
        { title: 'By Project', data: statsStore.data.byProject },
        { title: 'By Role', data: statsStore.data.byRole },
        { title: 'By Mode', data: statsStore.data.byMode },
        { title: 'By Model', data: statsStore.data.byModel },
      ] as section (section.title)}
        <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
          <h3 class="mb-3 text-sm font-semibold text-surface-500">{section.title}</h3>
          {#if section.data && section.data.length > 0}
            <table class="w-full text-sm">
              <thead>
                <tr class="border-b border-surface-200 text-left text-xs text-surface-400 dark:border-surface-700">
                  <th class="pb-2">Name</th>
                  <th class="pb-2 text-right">Runs</th>
                  <th class="pb-2 text-right">Cost</th>
                  <th class="pb-2 text-right">Duration</th>
                </tr>
              </thead>
              <tbody>
                {#each section.data as row (row.key)}
                  <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                    <td class="py-1.5 font-mono text-xs">{row.key}</td>
                    <td class="py-1.5 text-right">{row.stats.totalRuns}</td>
                    <td class="py-1.5 text-right">${row.stats.totalCostUsd.toFixed(2)}</td>
                    <td class="py-1.5 text-right">{formatDuration(row.stats.totalDurationS)}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          {:else}
            <p class="text-xs text-surface-400">No data</p>
          {/if}
        </div>
      {/each}
    </div>

    <!-- Recent runs -->
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <h3 class="mb-3 text-sm font-semibold text-surface-500">Recent Runs</h3>
      {#if statsStore.data.recentRuns && statsStore.data.recentRuns.length > 0}
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-surface-200 text-left text-xs text-surface-400 dark:border-surface-700">
                <th class="pb-2">Time</th>
                <th class="pb-2">Task</th>
                <th class="pb-2">Role</th>
                <th class="pb-2">Mode</th>
                <th class="pb-2">Model</th>
                <th class="pb-2 text-right">Cost</th>
                <th class="pb-2 text-right">Duration</th>
                <th class="pb-2">Outcome</th>
              </tr>
            </thead>
            <tbody>
              {#each statsStore.data.recentRuns as run (run.id)}
                <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                  <td class="py-1.5 text-xs text-surface-500">{formatDate(run.timestamp)}</td>
                  <td class="py-1.5 font-mono text-xs">{run.taskId}</td>
                  <td class="py-1.5">
                    <span class="rounded px-1.5 py-0.5 text-xs {roleBadgeClasses(run.role)}">{run.role}</span>
                  </td>
                  <td class="py-1.5 text-xs">{run.mode}</td>
                  <td class="py-1.5 text-xs">{run.model || '—'}</td>
                  <td class="py-1.5 text-right text-xs">${run.costUsd.toFixed(4)}</td>
                  <td class="py-1.5 text-right text-xs">{formatDuration(run.durationS)}</td>
                  <td class="py-1.5">
                    <span class="rounded px-1.5 py-0.5 text-xs {run.outcome === 'completed'
                      ? 'bg-success-200 text-success-800 dark:bg-success-800 dark:text-success-200'
                      : 'bg-error-200 text-error-800 dark:bg-error-800 dark:text-error-200'}">
                      {run.outcome}
                    </span>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <p class="text-xs text-surface-400">No runs recorded yet</p>
      {/if}
    </div>
  {/if}
</div>
