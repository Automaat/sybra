<script lang="ts">
  import { evaluationStore } from '../stores/evaluation.svelte.js'
  import { lifecycleStore } from '../stores/lifecycle.svelte.js'

  const report = $derived(evaluationStore.data)
  const phases = $derived(lifecycleStore.data)
  // Share is share-of-lead-time across the cohort, so the denominator is the
  // summed time in each phase (totalH) — not meanH, which averages only over
  // tasks that entered the phase and would over-weight phases many tasks skip.
  const phaseTotalSum = $derived(
    (phases?.phases ?? []).reduce((acc, p) => acc + p.totalH, 0),
  )
  // Stable colour per phase so the bar and legend agree across renders.
  const phaseColor: Record<string, string> = {
    queued: 'bg-surface-400',
    planning: 'bg-tertiary-500',
    implementing: 'bg-primary-500',
    testing: 'bg-secondary-500',
    review: 'bg-warning-500',
    waiting: 'bg-error-500',
    other: 'bg-surface-300',
  }
  function phaseShare(totalH: number): number {
    return phaseTotalSum > 0 ? (totalH / phaseTotalSum) * 100 : 0
  }
  function phaseBreakdown(byPhase: { [_ in string]?: number }): string {
    const order = ['queued', 'planning', 'implementing', 'testing', 'review', 'waiting', 'other']
    return order
      .filter((p) => (byPhase[p] ?? 0) > 0)
      .map((p) => `${p} ${(byPhase[p] ?? 0).toFixed(1)}`)
      .join(' · ')
  }
  // A zero-value Report (service disabled / no data yet) still has a non-null
  // overall, which would render as a measured "0% / failing" fleet. Treat it as
  // no-data unless there's at least one run or landing in the window.
  const hasData = $derived(
    !!report && !!report.overall && (report.overall.agentRuns > 0 || report.overall.tasksLanded > 0),
  )
  const o = $derived(hasData && report ? report.overall : null)

  $effect(() => {
    evaluationStore.load()
    evaluationStore.listen()
    lifecycleStore.load()
    return () => evaluationStore.stopListening()
  })

  function pct(x: number | undefined): string {
    return x === undefined ? '—' : `${(x * 100).toFixed(0)}%`
  }
  function hours(x: number | undefined): string {
    return x === undefined ? '—' : `${x.toFixed(1)}h`
  }
  function seconds(x: number | undefined): string {
    if (x === undefined) return '—'
    if (x >= 3600) return `${(x / 3600).toFixed(1)}h`
    if (x >= 60) return `${(x / 60).toFixed(1)}m`
    return `${x.toFixed(0)}s`
  }
  function num(x: number | undefined, digits = 1): string {
    return x === undefined ? '—' : x.toFixed(digits)
  }
  // Higher is better for these; render the bar/colour accordingly.
  function goodScale(x: number): string {
    if (x >= 0.8) return 'text-success-600 dark:text-success-400'
    if (x >= 0.5) return 'text-warning-600 dark:text-warning-400'
    return 'text-error-600 dark:text-error-400'
  }
  function sevClasses(sev: string): string {
    return sev === 'warn'
      ? 'bg-warning-200 text-warning-800 dark:bg-warning-800 dark:text-warning-200'
      : 'bg-surface-200 text-surface-700 dark:bg-surface-700 dark:text-surface-200'
  }
</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <div class="flex items-center justify-between">
    <p class="text-xs text-surface-400">
      {#if o}
        Fleet scorecard · last {o.windowDays} days
      {/if}
    </p>
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 disabled:opacity-50 dark:bg-surface-700 dark:hover:bg-surface-600"
      disabled={evaluationStore.loading}
      onclick={() => {
        evaluationStore.load()
        lifecycleStore.load()
      }}
    >
      {evaluationStore.loading ? 'Loading…' : 'Refresh'}
    </button>
  </div>

  {#if evaluationStore.error}
    <p class="text-error-500">{evaluationStore.error}</p>
  {/if}

  {#if o}
    <!-- Headline scorecard -->
    <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Tasks landed</span>
        <p class="mt-1 text-2xl font-bold">{o.tasksLanded}</p>
        <p class="mt-0.5 text-xs text-surface-400">{o.merged} merged · {o.mergedWithEdits} edited · {o.closed} closed</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Autonomy</span>
        <p class="mt-1 text-2xl font-bold {goodScale(o.autonomyRate)}">{pct(o.autonomyRate)}</p>
        <p class="mt-0.5 text-xs text-surface-400">{o.humanTouchedLandings} needed a human</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">CI first pass</span>
        <p class="mt-1 text-2xl font-bold {goodScale(o.ciFirstPassRate)}">{pct(o.ciFirstPassRate)}</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Failure rate</span>
        <p class="mt-1 text-2xl font-bold {goodScale(1 - o.failureRate)}">{pct(o.failureRate)}</p>
        <p class="mt-0.5 text-xs text-surface-400">{o.agentFailures}/{o.agentRuns} runs · {o.reverted} reverted ({pct(o.changeFailureRate)})</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Cost / landed</span>
        <p class="mt-1 text-2xl font-bold">${num(o.costPerLanded, 2)}</p>
        <p class="mt-0.5 text-xs text-surface-400">${num(o.totalCostUsd, 2)} total</p>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <span class="text-xs font-medium text-surface-500">Rework tasks</span>
        <p class="mt-1 text-2xl font-bold">{o.reworkTasks}</p>
      </div>
    </div>

    <!-- Throughput & efficiency detail -->
    <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <h3 class="mb-3 text-sm font-semibold text-surface-500">Cycle time</h3>
        <table class="w-full text-sm">
          <tbody>
            <tr class="border-b border-surface-100 dark:border-surface-700">
              <td class="py-1.5 text-surface-500">Lead time (created → landed)</td>
              <td class="py-1.5 text-right">p50 {hours(o.leadTimeP50H)} · p90 {hours(o.leadTimeP90H)}</td>
            </tr>
            <tr>
              <td class="py-1.5 text-surface-500">Cycle time (first run → landed)</td>
              <td class="py-1.5 text-right">p50 {hours(o.cycleTimeP50H)} · p90 {hours(o.cycleTimeP90H)}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <h3 class="mb-3 text-sm font-semibold text-surface-500">Effort per landed PR</h3>
        <table class="w-full text-sm">
          <tbody>
            <tr class="border-b border-surface-100 dark:border-surface-700">
              <td class="py-1.5 text-surface-500">Turns / landed</td>
              <td class="py-1.5 text-right">{num(o.turnsPerLanded)}</td>
            </tr>
            <tr>
              <td class="py-1.5 text-surface-500">Tools / landed</td>
              <td class="py-1.5 text-right">{num(o.toolsPerLanded)}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Where time goes: lead time decomposed by lifecycle phase -->
    {#if phases && phases.cohort > 0 && phases.phases.length > 0}
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <div class="mb-3 flex items-baseline justify-between">
          <h3 class="text-sm font-semibold text-surface-500">Where time goes</h3>
          <span class="text-xs text-surface-400">{phases.cohort} landed tasks</span>
        </div>

        <!-- Stacked share-of-lead-time bar -->
        <div class="mb-3 flex h-3 w-full overflow-hidden rounded-full">
          {#each phases.phases as p (p.phase)}
            <div
              class="{phaseColor[p.phase] ?? 'bg-surface-300'} h-full"
              style="width: {phaseShare(p.totalH)}%"
              title="{p.phase}: {phaseShare(p.totalH).toFixed(0)}%"
            ></div>
          {/each}
        </div>

        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-surface-200 text-left text-xs text-surface-400 dark:border-surface-700">
              <th class="pb-2">Phase</th>
              <th class="pb-2 text-right">Share</th>
              <th class="pb-2 text-right">p50</th>
              <th class="pb-2 text-right">p90</th>
              <th class="pb-2 text-right">Mean</th>
              <th class="pb-2 text-right">Tasks</th>
            </tr>
          </thead>
          <tbody>
            {#each phases.phases as p (p.phase)}
              <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                <td class="py-1.5">
                  <span class="mr-2 inline-block h-2 w-2 rounded-full {phaseColor[p.phase] ?? 'bg-surface-300'}"></span>
                  {p.phase}
                </td>
                <td class="py-1.5 text-right">{phaseShare(p.totalH).toFixed(0)}%</td>
                <td class="py-1.5 text-right">{hours(p.p50h)}</td>
                <td class="py-1.5 text-right text-surface-400">{hours(p.p90h)}</td>
                <td class="py-1.5 text-right">{hours(p.meanH)}</td>
                <td class="py-1.5 text-right text-surface-400">{p.count}</td>
              </tr>
            {/each}
          </tbody>
        </table>

        {#if phases.slowest && phases.slowest.length > 0}
          <h4 class="mb-2 mt-4 text-xs font-semibold text-surface-400">Slowest landed tasks</h4>
          <ul class="flex flex-col gap-1.5">
            {#each phases.slowest as t (t.taskId)}
              <li class="flex items-baseline justify-between gap-3 text-xs">
                <span class="font-mono text-surface-500">{t.taskId}</span>
                <span class="flex-1 truncate text-surface-400">{phaseBreakdown(t.byPhase)}</span>
                <span class="shrink-0 font-medium">{hours(t.totalH)}</span>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
    {/if}

    <!-- Weaknesses / feedback loop -->
    {#if report?.weaknesses && report.weaknesses.length > 0}
      <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
        <h3 class="mb-3 text-sm font-semibold text-surface-500">What to improve</h3>
        <ul class="flex flex-col gap-3">
          {#each report.weaknesses as w (w.metric)}
            <li class="flex flex-col gap-1">
              <div class="flex items-center gap-2">
                <span class="rounded px-1.5 py-0.5 text-xs {sevClasses(w.severity)}">{w.metric}</span>
                <span class="text-sm">{w.detail}</span>
              </div>
              <p class="pl-1 text-xs text-surface-400">→ {w.suggestion}</p>
            </li>
          {/each}
        </ul>
      </div>
    {/if}

    <!-- Breakdowns -->
    <div class="grid grid-cols-1 gap-6 lg:grid-cols-2">
      {#each [
        { title: 'By Provider', data: report?.byProvider },
        { title: 'By Role', data: report?.byRole },
      ] as section (section.title)}
        <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
          <h3 class="mb-3 text-sm font-semibold text-surface-500">{section.title}</h3>
          {#if section.data && section.data.length > 0}
            <table class="w-full text-sm">
              <thead>
                <tr class="border-b border-surface-200 text-left text-xs text-surface-400 dark:border-surface-700">
                  <th class="pb-2">Name</th>
                  <th class="pb-2 text-right">Runs</th>
                  <th class="pb-2 text-right">Fail %</th>
                  <th class="pb-2 text-right">Cost</th>
                  <th class="pb-2 text-right">Turns</th>
                  <th class="pb-2 text-right">Tools</th>
                </tr>
              </thead>
              <tbody>
                {#each section.data as row (row.key)}
                  <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                    <td class="py-1.5 font-mono text-xs">{row.key}</td>
                    <td class="py-1.5 text-right">{row.runs}</td>
                    <td class="py-1.5 text-right">{pct(row.failureRate)}</td>
                    <td class="py-1.5 text-right">${row.totalCostUsd.toFixed(2)}</td>
                    <td class="py-1.5 text-right text-surface-400">{row.turns}</td>
                    <td class="py-1.5 text-right text-surface-400">{row.tools}</td>
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

    <!-- A/B testing and model comparisons -->
    <div class="grid grid-cols-1 gap-6">
      {#each [
        { title: 'Agent / Model', data: report?.byAgentModel },
        { title: 'A/B Experiments', data: report?.byVariant },
      ] as section (section.title)}
        <div class="overflow-x-auto rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
          <h3 class="mb-3 text-sm font-semibold text-surface-500">{section.title}</h3>
          {#if section.data && section.data.length > 0}
            <table class="w-full min-w-[980px] text-sm">
              <thead>
                <tr class="border-b border-surface-200 text-left text-xs text-surface-400 dark:border-surface-700">
                  <th class="pb-2">Variant</th>
                  <th class="pb-2">Role</th>
                  <th class="pb-2 text-right">Runs</th>
                  <th class="pb-2 text-right">Landed</th>
                  <th class="pb-2 text-right">Merge %</th>
                  <th class="pb-2 text-right">CI 1st</th>
                  <th class="pb-2 text-right">Edited</th>
                  <th class="pb-2 text-right">Rework</th>
                  <th class="pb-2 text-right">Revert</th>
                  <th class="pb-2 text-right">Duration</th>
                  <th class="pb-2 text-right">Cost</th>
                  <th class="pb-2 text-right">Premium req</th>
                </tr>
              </thead>
              <tbody>
                {#each section.data as row (row.key)}
                  <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                    <td class="py-1.5">
                      <div class="font-mono text-xs">{row.variantId || row.key}</div>
                      <div class="text-xs text-surface-400">
                        {row.provider}{row.model ? ` · ${row.model}` : ''}{row.reasoningEffort ? ` · ${row.reasoningEffort}` : ''}
                      </div>
                    </td>
                    <td class="py-1.5 text-xs">{row.role || '—'}</td>
                    <td class="py-1.5 text-right">
                      {row.runs}
                      {#if row.insufficientData}
                        <span class="ml-1 rounded bg-surface-200 px-1 text-[10px] text-surface-500 dark:bg-surface-700">low N</span>
                      {/if}
                    </td>
                    <td class="py-1.5 text-right">{row.landed}</td>
                    <td class="py-1.5 text-right">{pct(row.mergeRate)}</td>
                    <td class="py-1.5 text-right">{pct(row.ciFirstPassRate)}</td>
                    <td class="py-1.5 text-right">{pct(row.mergedWithEditsRate)}</td>
                    <td class="py-1.5 text-right">{pct(row.reworkRate)}</td>
                    <td class="py-1.5 text-right">{pct(row.revertRate)}</td>
                    <td class="py-1.5 text-right">p50 {seconds(row.durationP50S)} · p90 {seconds(row.durationP90S)}</td>
                    <td class="py-1.5 text-right">${num(row.costPerLanded, 2)}/landed</td>
                    <td class="py-1.5 text-right">{num(row.premiumRequestsPerLanded, 1)}/landed</td>
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

    {#if report?.notes && report.notes.length > 0}
      <div class="rounded-lg border border-dashed border-surface-300 p-4 text-xs text-surface-400 dark:border-surface-600">
        <p class="mb-1 font-semibold">Deferred metrics</p>
        <ul class="list-inside list-disc">
          {#each report.notes as note (note)}
            <li>{note}</li>
          {/each}
        </ul>
      </div>
    {/if}
  {:else if !evaluationStore.error}
    <p class="text-sm text-surface-400">No evaluation data yet.</p>
  {/if}
</div>
