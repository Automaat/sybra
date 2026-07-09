<script lang="ts">
  import { onMount } from 'svelte'
  import { GetMonitorReport, EventsOn } from '$lib/api'
  import { MonitorReport } from '$lib/events.js'
  import { formatCostShort } from '$lib/cost.js'
  import { activeTaskNeedsUserAttention } from '$lib/task-attention.js'
  import type { MonitorReportBinding } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import type { Report } from '../../bindings/github.com/Automaat/sybra/internal/monitor/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { statsStore } from '../stores/stats.svelte.js'

  let monitorBinding = $state<MonitorReportBinding | null>(null)
  let monitorFetchFailed = $state(false)

  // Isolated panel — the only one allowed to read stepTexts. A stream event
  // touches only this list's re-render, never the queue-depth/needs-you
  // aggregates below (those are derived off taskStore alone).
  const runningAgents = $derived(agentStore.list.filter((a) => a.state === 'running'))

  const queueDepth = $derived(taskStore.byStatus('todo').length)
  const needsYouCount = $derived(taskStore.list.filter(activeTaskNeedsUserAttention).length)

  // Three honest states: no data yet and no error ("unavailable"), a refresh
  // failure that must not silently pass off prior data as current ("stale"),
  // or a genuine live load.
  const statsState = $derived.by((): 'absent' | 'stale' | 'live' => {
    if (statsStore.error) return 'stale'
    if (statsStore.data === null) return 'absent'
    return 'live'
  })
  const todaysBurn = $derived(statsStore.data?.today?.totalCostUsd ?? 0)
  const limitProviders = $derived(statsStore.data?.limits?.providers ?? [])

  // Monitor tri-state: disabled / not-ready ("waiting") / ready with a drift
  // count. A caught GetMonitorReport() rejection is a fourth, harder failure
  // that must never collapse into "0 drift".
  const monitorReport = $derived<Report | null>(
    monitorBinding?.ready ? monitorBinding.report : null,
  )
  const driftCount = $derived(monitorReport?.anomalies?.length ?? 0)

  function providerLabel(provider: string): string {
    switch (provider) {
      case 'claude': return 'Claude'
      case 'codex': return 'Codex'
      case 'copilot': return 'Copilot'
      default: return provider || 'Provider'
    }
  }

  onMount(() => {
    void statsStore.load()

    GetMonitorReport()
      .then((binding: MonitorReportBinding) => {
        monitorBinding = binding
      })
      .catch(() => {
        monitorFetchFailed = true
      })

    const unsubMonitor = EventsOn(MonitorReport, (report: Report) => {
      monitorFetchFailed = false
      monitorBinding = { enabled: true, ready: true, report } as MonitorReportBinding
    })

    return () => {
      unsubMonitor()
    }
  })
</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <h2 class="text-lg font-semibold">Fleet Health</h2>

  <div class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
    <!-- Running agents -->
    <section class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <h3 class="mb-3 text-sm font-semibold text-surface-500">Running Agents</h3>
      {#if runningAgents.length === 0}
        <p class="text-xs text-surface-400">No agents running</p>
      {:else}
        <ul class="flex flex-col gap-2">
          {#each runningAgents as a (a.id)}
            <li class="flex flex-col gap-0.5 rounded border border-surface-200 px-2 py-1.5 text-xs dark:border-surface-700">
              <span class="font-medium">{a.name || a.taskId || a.id}</span>
              <span class="truncate text-surface-400">{agentStore.stepTexts.get(a.id) ?? '…'}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>

    <!-- Queue depth / needs-you -->
    <section class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <h3 class="mb-3 text-sm font-semibold text-surface-500">Board</h3>
      <div class="flex items-center justify-between text-sm">
        <span class="text-surface-500">Queue depth</span>
        <span class="font-bold">{queueDepth}</span>
      </div>
      <div class="mt-2 flex items-center justify-between text-sm">
        <span class="text-surface-500">Needs you</span>
        <span class="font-bold {needsYouCount > 0 ? 'text-error-500' : ''}">{needsYouCount}</span>
      </div>
    </section>

    <!-- Today's burn + provider limits -->
    <section class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <h3 class="mb-3 text-sm font-semibold text-surface-500">Burn &amp; Limits</h3>
      {#if statsState === 'absent'}
        <p class="text-xs text-surface-400">Stats unavailable</p>
      {:else}
        {#if statsState === 'stale'}
          <p class="mb-2 text-xs text-warning-600 dark:text-warning-300">
            Stats refresh failed — showing last known data (stale)
          </p>
        {/if}
        <div class="flex items-center justify-between text-sm">
          <span class="text-surface-500">Today's burn</span>
          <span class="font-bold">{formatCostShort(todaysBurn)}</span>
        </div>
        {#if limitProviders.length > 0}
          <ul class="mt-3 flex flex-col gap-1.5">
            {#each limitProviders as p (p.provider)}
              <li class="flex items-center justify-between text-xs">
                <span>{providerLabel(p.provider)}</span>
                <span class="text-surface-500">
                  {p.quotaLimited ? 'limited' : `${(p.sessionUsedPercent ?? 0).toFixed(0)}% session`}
                </span>
              </li>
            {/each}
          </ul>
        {:else}
          <p class="mt-2 text-xs text-surface-400">No provider limits reported</p>
        {/if}
      {/if}
    </section>

    <!-- Monitor drift -->
    <section class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <h3 class="mb-3 text-sm font-semibold text-surface-500">Monitor</h3>
      {#if monitorFetchFailed}
        <p class="text-xs text-error-500">monitor unavailable</p>
      {:else if monitorBinding && !monitorBinding.enabled}
        <p class="text-xs text-surface-400">monitor disabled</p>
      {:else if !monitorBinding || !monitorBinding.ready}
        <p class="text-xs text-surface-400">waiting</p>
      {:else}
        <div class="flex items-center justify-between text-sm">
          <span class="text-surface-500">Drift</span>
          <span class="font-bold {driftCount > 0 ? 'text-warning-500' : ''}">{driftCount}</span>
        </div>
      {/if}
    </section>
  </div>
</div>
