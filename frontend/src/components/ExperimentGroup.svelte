<script lang="ts">
  import { interpretExperiment } from '$lib/evaluation-interpretation.js'
  import {
    estimateBasis,
    guardrailClasses,
    num,
    rateCell,
    sampleClasses,
    sampleLabel,
    seconds,
    verdictClasses,
    type ComparisonRowLike,
  } from '$lib/evaluation-format.js'
  import type {
    ComparisonBreakdown,
    ExperimentKindBreakdown,
  } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'

  let { kind, title, breakdown, emptyMessage }: {
    kind: string
    title: string
    breakdown: ExperimentKindBreakdown | undefined
    emptyMessage?: string
  } = $props()

  const groups = $derived(breakdown?.groups ?? [])
  const resolvedEmptyMessage = $derived(emptyMessage ?? `No ${title.toLowerCase()} configured.`)

  function subjectLabel(row: ComparisonBreakdown): string {
    const s = row.subject
    if (!s) return ''
    return [s.workflowId, s.stepId, s.role, s.skillName].filter(Boolean).join(' · ')
  }

  function rowState(row: ComparisonRowLike): string {
    if (row.baseline) {
      // Surface the caveat on the baseline row too — a parity-unknown/low-sample
      // baseline is an untrustworthy comparison basis and must not read as clean.
      if (row.sampleStatus && row.sampleStatus !== 'actionable') {
        return `baseline · ${sampleLabel(row.sampleStatus)}`
      }
      return 'baseline'
    }
    if (row.baselineVariantId && !row.failureEstimate?.hasDelta) return 'no baseline'
    return sampleLabel(row.sampleStatus)
  }

  // Applied to the sample-size badge and any breached guardrail chip so the
  // caveat visually outweighs the metric it qualifies, per AC3 — a low-N or
  // guardrail-breach row should never read as quietly as a healthy one.
  function isDominantSample(row: ComparisonRowLike): boolean {
    return row.sampleStatus === 'low-sample' || row.sampleStatus === 'parity-unknown' || row.sampleStatus === 'low-sample+parity-unknown'
  }

  function parityUnknownVariants(exp: { variants?: { sampleStatus?: string }[] }): number {
    return (exp.variants ?? []).filter((variant) =>
      variant.sampleStatus === 'parity-unknown' || variant.sampleStatus === 'low-sample+parity-unknown').length
  }
</script>

<div class="overflow-x-auto rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
  <h3 class="mb-1 text-sm font-semibold text-surface-500">{title}</h3>
  {#if kind === 'unknown'}
    <p class="mb-3 max-w-3xl text-xs text-warning-600 dark:text-warning-400">
      These rows reference an experiment ID no longer present in configuration (orphaned).
    </p>
  {:else}
    <p class="mb-3 max-w-3xl text-xs text-surface-400">
      Primary signal: landed/run. Guardrails watch reliability, quality, speed, cost, and premium-model usage.
    </p>
  {/if}

  {#if !breakdown}
    <p class="text-xs text-surface-400">{resolvedEmptyMessage}</p>
  {:else if groups.length === 0}
    <p class="text-xs text-surface-400">{title} configured, but no runs recorded yet.</p>
  {:else}
    {#each groups as expGroup (expGroup.experimentId)}
      <div class="mb-4 border-t border-surface-200 pt-3 first:mt-0 first:border-0 first:pt-0 dark:border-surface-700">
        <h4 class="mb-1 text-xs font-semibold text-surface-500">
          {expGroup.experimentId}
          {#if expGroup.subject}
            <span class="font-normal text-surface-400">
              · {[expGroup.subject.workflowId, expGroup.subject.stepId, expGroup.subject.role, expGroup.subject.skillName].filter(Boolean).join(' · ')}
            </span>
          {/if}
        </h4>

        {#if (expGroup.rows?.length ?? 0) === 0 && (expGroup.rowsContribution?.length ?? 0) === 0}
          <p class="text-xs text-surface-400">Configured, but no runs recorded yet.</p>
        {:else}
          {#if expGroup.experiments && expGroup.experiments.length > 0}
            <div class="mb-4 grid gap-2 md:grid-cols-2 xl:grid-cols-3">
              {#each expGroup.experiments as exp (exp.key)}
                <div class="rounded border border-surface-200 p-3 text-xs dark:border-surface-700">
                  <div class="mb-1 flex items-center justify-between gap-2">
                    <span class="truncate font-mono">{exp.experimentId} · {exp.role}</span>
                    <span class="rounded px-1.5 py-0.5 {sampleClasses(exp.status)} {exp.status === 'low-sample' ? 'experiment-caveat--dominant' : ''}">{exp.status}</span>
                  </div>
                  <div class="text-surface-400">
                    {exp.readyVariants}/{exp.variants.length} ready · {exp.totalRuns} runs · min {exp.minSamplesPerVariant}
                  </div>
                  {#if parityUnknownVariants(exp) > 0}
                    <div class="mt-1 text-warning-700 dark:text-warning-300">
                      {parityUnknownVariants(exp)} parity-unknown variant{parityUnknownVariants(exp) === 1 ? '' : 's'}
                    </div>
                  {/if}
                  {#if exp.baselineVariantId}
                    <div class="mt-1 text-surface-400">baseline {exp.baselineVariantId}</div>
                  {/if}
                </div>
              {/each}
            </div>
          {/if}

          {#snippet comparisonTable(rows: ComparisonBreakdown[], peers: ComparisonBreakdown[], contribution: boolean)}
            <table class="w-full min-w-[1180px] text-sm">
              <thead>
                <tr class="border-b border-surface-200 text-left text-xs text-surface-400 dark:border-surface-700">
                  <th class="pb-2">Variant</th>
                  <th class="pb-2">Role</th>
                  <th class="pb-2 text-right">Runs</th>
                  <th class="pb-2 text-right">Landed</th>
                  <th class="pb-2 text-right">Fail %</th>
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
                {#each rows as row (row.key)}
                  {@const interpretation = interpretExperiment(row, peers)}
                  <tr class="border-b border-surface-100 bg-surface-100/60 dark:border-surface-700 dark:bg-surface-700/30">
                    <td class="py-1.5">
                      <div class="font-mono text-xs">{row.variantId || row.key}</div>
                      <div class="text-xs text-surface-400">
                        {row.provider}{row.model ? ` · ${row.model}` : ''}{row.reasoningEffort ? ` · ${row.reasoningEffort}` : ''}
                      </div>
                      {#if subjectLabel(row)}
                        <div class="text-xs text-surface-400">{subjectLabel(row)}</div>
                      {/if}
                      <div class="mt-1 flex flex-wrap items-center gap-1.5">
                        <span class="rounded px-1.5 py-0.5 text-[10px] font-medium {verdictClasses(interpretation.verdict)}">
                          {interpretation.verdictLabel}
                        </span>
                        <span class="text-[10px] text-surface-500">
                          {interpretation.primaryLabel}: {interpretation.primaryValue} ({interpretation.primaryDetail})
                        </span>
                      </div>
                      <p class="mt-1 text-[10px] text-surface-400">{interpretation.verdictReason}</p>
                    </td>
                    <td class="py-1.5 text-xs font-medium">All roles</td>
                    <td class="py-1.5 text-right">
                      {row.runs}
                      {#if row.stalled > 0}
                        <span class="ml-1 rounded bg-surface-200 px-1 text-[10px] text-surface-500 dark:bg-surface-700" title="retried, excluded from the failure rate">{row.stalled} stalled</span>
                      {/if}
                      {#if rowState(row)}
                        <span class="ml-1 rounded px-1 text-[10px] {sampleClasses(row.sampleStatus)} {isDominantSample(row) ? 'experiment-caveat--dominant' : ''}">{rowState(row)}</span>
                      {/if}
                    </td>
                    <td class="py-1.5 text-right text-xs">
                      {#if contribution}
                        {row.landed}
                        {#if row.qualityAttributionLimited}
                          <span class="ml-1 rounded bg-warning-200 px-1 text-[10px] text-warning-800 dark:bg-warning-800 dark:text-warning-200">limited attribution</span>
                        {/if}
                      {:else}
                        {row.landed} · {rateCell(row, 'landedEstimate', undefined, true)}
                      {/if}
                    </td>
                    <td class="py-1.5 text-right text-xs" title={estimateBasis(row, 'failureEstimate')}>{rateCell(row, 'failureEstimate', row.failureRate, true)}</td>
                    <td class="py-1.5 text-right text-xs">{rateCell(row, 'mergeEstimate', row.mergeRate, true)}</td>
                    <td class="py-1.5 text-right text-xs">{rateCell(row, 'ciFirstPassEstimate', row.ciFirstPassRate, true)}</td>
                    <td class="py-1.5 text-right text-xs">{rateCell(row, 'mergedWithEditsEstimate', row.mergedWithEditsRate, true)}</td>
                    <td class="py-1.5 text-right text-xs">{rateCell(row, 'reworkEstimate', row.reworkRate, true)}</td>
                    <td class="py-1.5 text-right text-xs">{rateCell(row, 'revertEstimate', row.revertRate, true)}</td>
                    <td class="py-1.5 text-right">p50 {seconds(row.durationP50S)} · p90 {seconds(row.durationP90S)}</td>
                    <td class="py-1.5 text-right">${num(row.totalCostUsd, 2)} · ${num(row.costPerLanded, 2)}/landed</td>
                    <td class="py-1.5 text-right">{num(row.premiumRequests, 1)} · {num(row.premiumRequestsPerLanded, 1)}/landed</td>
                  </tr>
                  <tr class="border-b border-surface-100 dark:border-surface-700">
                    <td class="pb-2 pt-0" colspan="13">
                      <div class="flex flex-wrap gap-1.5">
                        {#each interpretation.guardrails as guardrail (guardrail.key)}
                          <span
                            class="rounded border px-1.5 py-0.5 text-[10px] {guardrailClasses(guardrail.status)} {guardrail.status === 'breach' ? 'experiment-caveat--dominant' : ''}"
                            title={guardrail.detail}
                          >
                            {guardrail.label}: {guardrail.status}
                          </span>
                        {/each}
                      </div>
                      {#if interpretation.limitedSignals.length > 0}
                        <p class="mt-1 text-[10px] text-surface-400">
                          Limited signals: {interpretation.limitedSignals.join(' ')}
                        </p>
                      {/if}
                    </td>
                  </tr>
                  {#each row.roleBreakdowns ?? [] as child (child.key)}
                    <tr class="border-b border-surface-100 last:border-0 dark:border-surface-700">
                      <td class="py-1.5 pl-6">
                        <div class="font-mono text-xs text-surface-500">{child.variantId || row.variantId || child.key}</div>
                        <div class="text-xs text-surface-400">
                          {child.provider}{child.model ? ` · ${child.model}` : ''}{child.reasoningEffort ? ` · ${child.reasoningEffort}` : ''}
                        </div>
                      </td>
                      <td class="py-1.5 text-xs">{child.role || '—'}</td>
                      <td class="py-1.5 text-right">
                        {child.runs}
                        {#if child.stalled > 0}
                          <span class="ml-1 rounded bg-surface-200 px-1 text-[10px] text-surface-500 dark:bg-surface-700" title="retried, excluded from the failure rate">{child.stalled} stalled</span>
                        {/if}
                        {#if rowState(child)}
                          <span class="ml-1 rounded px-1 text-[10px] {sampleClasses(child.sampleStatus)} {isDominantSample(child) ? 'experiment-caveat--dominant' : ''}">{rowState(child)}</span>
                        {/if}
                      </td>
                      <td class="py-1.5 text-right text-xs">
                        {#if contribution}
                          <span class="text-xs">{child.landed} · {rateCell(child, 'landedEstimate', undefined, true)}</span>
                          {#if child.qualityAttributionLimited}
                            <span class="ml-1 rounded bg-warning-200 px-1 text-[10px] text-warning-800 dark:bg-warning-800 dark:text-warning-200">limited attribution</span>
                          {/if}
                        {:else}
                          {child.landed} · {rateCell(child, 'landedEstimate', undefined, true)}
                        {/if}
                      </td>
                      <td class="py-1.5 text-right text-xs" title={estimateBasis(child, 'failureEstimate')}>{rateCell(child, 'failureEstimate', child.failureRate, true)}</td>
                      <td class="py-1.5 text-right text-xs">{rateCell(child, 'mergeEstimate', child.mergeRate, true)}</td>
                      <td class="py-1.5 text-right text-xs">{rateCell(child, 'ciFirstPassEstimate', child.ciFirstPassRate, true)}</td>
                      <td class="py-1.5 text-right text-xs">{rateCell(child, 'mergedWithEditsEstimate', child.mergedWithEditsRate, true)}</td>
                      <td class="py-1.5 text-right text-xs">{rateCell(child, 'reworkEstimate', child.reworkRate, true)}</td>
                      <td class="py-1.5 text-right text-xs">{rateCell(child, 'revertEstimate', child.revertRate, true)}</td>
                      <td class="py-1.5 text-right">p50 {seconds(child.durationP50S)} · p90 {seconds(child.durationP90S)}</td>
                      <td class="py-1.5 text-right">${num(child.totalCostUsd, 2)} · ${num(child.costPerLanded, 2)}/landed</td>
                      <td class="py-1.5 text-right">{num(child.premiumRequests, 1)} · {num(child.premiumRequestsPerLanded, 1)}/landed</td>
                    </tr>
                  {/each}
                {/each}
              </tbody>
            </table>
          {/snippet}

          {#if expGroup.rows && expGroup.rows.length > 0}
            <h5 class="mb-2 mt-2 text-xs font-semibold text-surface-400">Latest author</h5>
            {@render comparisonTable(expGroup.rows, expGroup.rows, false)}
          {/if}

          {#if expGroup.rowsContribution && expGroup.rowsContribution.length > 0}
            <h5 class="mb-2 mt-4 text-xs font-semibold text-surface-400">Contribution</h5>
            <p class="mb-2 max-w-3xl text-xs text-surface-400">
              Credits each distinct in-window author group that contributed before landing; totals can exceed landed tasks.
            </p>
            {@render comparisonTable(expGroup.rowsContribution, expGroup.rowsContribution, true)}
          {/if}
        {/if}
      </div>
    {/each}
  {/if}
</div>

<style>
  /* Makes a low-sample or guardrail-breach caveat outweigh the metric it
     qualifies — bold text plus a visible ring, not just a colour tint that
     blends into the neighbouring badges. */
  :global(.experiment-caveat--dominant) {
    font-weight: 700;
    box-shadow: 0 0 0 1.5px currentColor;
  }
</style>
