<script lang="ts">
  import { statsStore } from '../stores/stats.svelte.js'
  import {
    periodCutoff,
    dailyCost,
    costByProject,
    closedTasksSeries,
    type StatsPeriod,
  } from '$lib/stats-charts.js'
  import StatsLineChart from '../components/stats/StatsLineChart.svelte'
  import StatsBarChart from '../components/stats/StatsBarChart.svelte'

  type Period = StatsPeriod
  let period = $state<Period>('allTime')
  let now = $state(new Date())

  const periods: { key: Period; label: string }[] = [
    { key: 'today', label: 'Today' },
    { key: 'thisWeek', label: 'This Week' },
    { key: 'thisMonth', label: 'This Month' },
    { key: 'allTime', label: 'All Time' },
  ]

  const periodLabel = $derived(periods.find((p) => p.key === period)?.label ?? '')

  const summary = $derived(
    statsStore.data ? statsStore.data[period] : null,
  )

  // Charts are derived from the recent-runs sample, filtered to the selected
  // period. The backend caps that sample at 50; when it's full, older runs are
  // not represented, so the captions flag it and the tables below stay
  // authoritative for full totals.
  const recentRuns = $derived(statsStore.data?.recentRuns ?? [])
  const limitProviders = $derived(statsStore.data?.limits?.providers ?? [])
  // The backend only caps recentRuns once there are MORE than 50 total runs.
  const sampleCapped = $derived((statsStore.data?.allTime?.totalRuns ?? 0) > 50)
  const cutoff = $derived(periodCutoff(period, now))
  const costSeries = $derived(dailyCost(recentRuns, cutoff, now).map((p) => ({ date: p.date, value: p.cost })))
  const taskSeries = $derived(closedTasksSeries(statsStore.data?.closedTasksDaily ?? [], cutoff, now))
  const projectCosts = $derived(costByProject(recentRuns, cutoff))

  $effect(() => {
    void refreshStats()
  })

  async function refreshStats(): Promise<void> {
    now = new Date()
    await statsStore.load()
  }

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

  function providerLabel(provider: string): string {
    switch (provider) {
      case 'claude': return 'Claude'
      case 'codex': return 'Codex'
      case 'copilot': return 'Copilot'
      default: return provider || 'Provider'
    }
  }

  function formatPercent(v?: number): string {
    if (v == null) return '—'
    return `${v.toFixed(v >= 10 ? 0 : 1)}%`
  }

  function progressWidth(v?: number): string {
    return `${Math.max(0, Math.min(100, v ?? 0))}%`
  }

  function resetLabel(ts?: any): string {
    if (!ts) return 'unknown reset'
    const d = new Date(ts)
    if (Number.isNaN(d.getTime())) return 'unknown reset'
    return `resets ${d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}`
  }

  function spendLine(sessionUsd?: number, weeklyUsd?: number): string {
    return `$${(sessionUsd ?? 0).toFixed(2)} session / $${(weeklyUsd ?? 0).toFixed(2)} week`
  }

  function subscriptionLine(monthly?: number, weekly?: number): string {
    if (!monthly) return ''
    const ratio = ((weekly ?? 0) / monthly) * 100
    return `${ratio.toFixed(0)}% of $${monthly.toFixed(0)}/mo`
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

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <div class="flex items-center justify-end">
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={refreshStats}
    >
      Refresh
    </button>
  </div>

  {#if statsStore.error}
    <p class="text-error-500">{statsStore.error}</p>
  {/if}

  <!-- Period tabs -->
  <div class="flex gap-1 rounded-lg bg-surface-200 p-1 dark:bg-surface-800">
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
    <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Tasks Done</span>
        <p class="mt-1 text-2xl font-bold">{summary.tasksDone}</p>
      </div>
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
        {#if summary.totalReasoningTokens}
          <p class="mt-0.5 text-xs text-surface-400">{formatTokens(summary.totalReasoningTokens)} reasoning</p>
        {/if}
      </div>
    </div>
  {/if}

  <!-- Charts -->
  {#if statsStore.data}
    {#if limitProviders.length > 0}
      <div class="space-y-3">
        <div class="mb-3 flex items-baseline justify-between gap-2">
          <h3 class="text-sm font-semibold text-surface-500">Agent Limits</h3>
          <span class="text-[10px] text-surface-400">session + weekly cycles</span>
        </div>
        <div class="grid grid-cols-1 gap-3 lg:grid-cols-3">
          {#each limitProviders as p (p.provider)}
            <div class="rounded border border-surface-200 bg-white p-3 dark:border-surface-700 dark:bg-surface-900">
              <div class="mb-2 flex items-center justify-between gap-2">
                <div>
                  <p class="text-sm font-semibold">{providerLabel(p.provider)}</p>
                  <p class="text-xs text-surface-500">{p.planType || p.confidence || 'usage only'}</p>
                </div>
                {#if p.quotaLimited}
                  <span class="rounded bg-warning-200 px-1.5 py-0.5 text-xs text-warning-800 dark:bg-warning-800 dark:text-warning-100">limited</span>
                {:else if p.confidence === 'exact'}
                  <span class="rounded bg-success-200 px-1.5 py-0.5 text-xs text-success-800 dark:bg-success-800 dark:text-success-100">exact</span>
                {:else}
                  <span class="rounded bg-surface-200 px-1.5 py-0.5 text-xs text-surface-600 dark:bg-surface-700 dark:text-surface-200">estimated</span>
                {/if}
              </div>
              <div class="space-y-2">
                <div>
                  <div class="mb-1 flex justify-between text-xs">
                    <span>Session</span>
                    <span>{formatPercent(p.sessionUsedPercent)}</span>
                  </div>
                  <div class="h-1.5 overflow-hidden rounded bg-surface-200 dark:bg-surface-700">
                    <div class="h-full bg-primary-500" style={`width: ${progressWidth(p.sessionUsedPercent)}`}></div>
                  </div>
                  <p class="mt-1 text-[10px] text-surface-400">{resetLabel(p.sessionResetsAt)}</p>
                </div>
                <div>
                  <div class="mb-1 flex justify-between text-xs">
                    <span>Weekly</span>
                    <span>{formatPercent(p.weeklyUsedPercent)}</span>
                  </div>
                  <div class="h-1.5 overflow-hidden rounded bg-surface-200 dark:bg-surface-700">
                    <div class="h-full bg-secondary-500" style={`width: ${progressWidth(p.weeklyUsedPercent)}`}></div>
                  </div>
                  <p class="mt-1 text-[10px] text-surface-400">{resetLabel(p.weeklyResetsAt)}</p>
                </div>
              </div>
              <div class="mt-3 border-t border-surface-100 pt-2 text-xs dark:border-surface-700">
                <p class="font-medium">{spendLine(p.sessionSpendUsd, p.weeklySpendUsd)}</p>
                <p class="text-surface-500">
                  {formatTokens(p.weeklyInputTokens ?? 0)} in / {formatTokens(p.weeklyOutputTokens ?? 0)} out
                  {#if p.weeklyPremiumRequests} · {p.weeklyPremiumRequests} premium{/if}
                </p>
                {#if p.monthlySubscriptionUsd}
                  <p class="text-surface-500">{subscriptionLine(p.monthlySubscriptionUsd, p.weeklySpendUsd)}</p>
                {/if}
                {#if p.quotaReason}
                  <p class="mt-1 text-warning-600 dark:text-warning-300">{p.quotaReason}</p>
                {/if}
              </div>
            </div>
          {/each}
        </div>
      </div>
    {/if}

    <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <div class="mb-3 flex items-baseline justify-between gap-2">
          <h3 class="text-sm font-semibold text-surface-500">Cost over time</h3>
          <span class="text-[10px] text-surface-400">{periodLabel}{#if sampleCapped} · recent 50 runs{/if}</span>
        </div>
        <StatsLineChart points={costSeries} ariaLabel="Cost over time" emptyLabel="No cost in this range" />
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <div class="mb-3 flex items-baseline justify-between gap-2">
          <h3 class="text-sm font-semibold text-surface-500">Cost by project</h3>
          <span class="text-[10px] text-surface-400">{periodLabel}{#if sampleCapped} · recent 50 runs{/if}</span>
        </div>
        <StatsBarChart bars={projectCosts} />
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <div class="mb-3 flex items-baseline justify-between gap-2">
          <h3 class="text-sm font-semibold text-surface-500">Closed tasks over time</h3>
          <span class="text-[10px] text-surface-400">{periodLabel}</span>
        </div>
        <StatsLineChart points={taskSeries} ariaLabel="Closed tasks over time" emptyLabel="No closed tasks in this range" />
      </div>
    </div>
  {/if}

  <!-- Breakdowns -->
  {#if statsStore.data}
    <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
      {#each [
        { title: 'By Project Type', data: statsStore.data.byProjectType },
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
                  <th class="pb-2 text-right">Reasoning</th>
                </tr>
              </thead>
              <tbody>
                {#each section.data as row (row.key)}
                  <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                    <td class="py-1.5 font-mono text-xs">{row.key}</td>
                    <td class="py-1.5 text-right">{row.stats.totalRuns}</td>
                    <td class="py-1.5 text-right">${row.stats.totalCostUsd.toFixed(2)}</td>
                    <td class="py-1.5 text-right">{formatDuration(row.stats.totalDurationS)}</td>
                    <td class="py-1.5 text-right text-surface-400">{row.stats.totalReasoningTokens ? formatTokens(row.stats.totalReasoningTokens) : '—'}</td>
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
                <th class="pb-2 text-right">Reasoning</th>
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
                  <td class="py-1.5 text-right text-xs text-surface-400">{run.reasoningTokens ? formatTokens(run.reasoningTokens) : '—'}</td>
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
