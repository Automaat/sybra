<script lang="ts">
  import { AlertTriangle, Info } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import type { TamperReportDTO } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import { GetTamperReport } from '$lib/api'
  import { statusSummary } from '../../lib/status-summary.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { timeAgo } from '$lib/dates.js'
  import { isTamperFlaggedTask } from '$lib/tamper.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  const summary = $derived(statusSummary(task.status))
  const freshness = $derived(timeAgo(task.updatedAt))
  const isTamperFlagged = $derived(isTamperFlaggedTask(task))
  // Attention sub-states (or any status_reason) warrant the warm banner; a
  // quiet folded sub-state uses neutral surface styling.
  const tone = $derived(summary?.tone === 'info' && !task.statusReason ? 'info' : 'attention')

  let report = $state<TamperReportDTO | null>(null)
  let reportLoading = $state(false)
  let reportError = $state('')
  let blessError = $state('')
  let reportSeq = 0
  const blessLoading = $derived(taskStore.isBlessing(task.id))

  $effect(() => {
    if (!isTamperFlagged) {
      report = null
      reportLoading = false
      reportError = ''
      return
    }

    const seq = ++reportSeq
    reportLoading = true
    reportError = ''
    report = null
    void GetTamperReport(task.id)
      .then((result) => {
        if (seq !== reportSeq) return
        report = result
      })
      .catch((e) => {
        if (seq !== reportSeq) return
        reportError = String(e)
      })
      .finally(() => {
        if (seq === reportSeq) reportLoading = false
      })

    return () => {
      reportSeq++
    }
  })

  async function blessTampering() {
    if (blessLoading || !isTamperFlagged) return
    blessError = ''
    try {
      await taskStore.blessTampering(task.id)
    } catch (e) {
      blessError = String(e)
    }
  }
</script>

{#if summary || task.statusReason}
  <div
    role="status"
    class="flex items-start gap-2 rounded-md border px-3 py-2 text-sm {tone === 'attention'
      ? 'border-warning-300 bg-warning-50 text-warning-800 dark:border-warning-700 dark:bg-warning-900/40 dark:text-warning-200'
      : 'border-surface-300 bg-surface-200 text-surface-700 dark:border-surface-600 dark:bg-surface-800 dark:text-surface-300'}"
  >
    {#if tone === 'attention'}
      <AlertTriangle size={16} class="mt-0.5 shrink-0" />
    {:else}
      <Info size={16} class="mt-0.5 shrink-0" />
    {/if}
    <div class="flex flex-1 flex-col gap-2">
      {#if summary}
        <span>
          <span class="font-semibold">{summary.label}</span>{#if summary.hint} — {summary.hint}{/if}{#if freshness}<span class="opacity-70"> · updated {freshness}</span>{/if}
        </span>
      {/if}
      {#if task.statusReason}
        <span class={summary ? 'opacity-90' : ''}>{task.statusReason}</span>
      {/if}
      {#if isTamperFlagged}
        <div class="mt-1 flex flex-col gap-2 rounded border border-warning-300 bg-warning-100/70 p-2 text-xs dark:border-warning-700 dark:bg-warning-950/40">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <span class="font-semibold">Tamper findings</span>
            <button
              type="button"
              class="rounded bg-success-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-success-700 disabled:opacity-50"
              onclick={blessTampering}
              disabled={blessLoading}
            >
              {blessLoading ? 'Blessing...' : 'Bless & send to review'}
            </button>
          </div>
          {#if reportLoading}
            <p class="opacity-80">Loading tamper report...</p>
          {:else if reportError}
            <p class="text-error-700 dark:text-error-300">Could not load tamper report: {reportError}</p>
          {:else if report && !report.reportAvailable}
            <p class="opacity-80">No tamper report artifact is available for this task.</p>
          {:else if report && report.findings.length === 0}
            <p class="opacity-80">The tamper report has no findings.</p>
          {:else if report}
            <ul class="flex flex-col gap-1">
              {#each report.findings as finding}
                <li class="rounded border border-warning-200 bg-white/60 p-2 dark:border-warning-800 dark:bg-surface-950/30">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-medium">{finding.file}</span>
                    <span class="rounded bg-warning-200 px-1.5 py-0.5 text-[11px] font-semibold uppercase text-warning-900 dark:bg-warning-800 dark:text-warning-100">{finding.severity}</span>
                    <span class="opacity-80">{finding.category}</span>
                    <span class="font-mono">{finding.rule}</span>
                  </div>
                  {#if finding.detail}
                    <p class="mt-1 opacity-90">{finding.detail}</p>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
          {#if blessError}
            <p class="text-error-700 dark:text-error-300">{blessError}</p>
          {/if}
        </div>
      {/if}
    </div>
  </div>
{/if}
