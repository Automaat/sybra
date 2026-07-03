<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { renderMarkdown } from '../../lib/markdown.js'
  import { runRoleLabel, runRoleClasses, runStateClasses } from '../../lib/agent-run.js'
  import { formatCostShort } from '../../lib/cost.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  const renderedReview = $derived(renderMarkdown(task.codeReview))

  // The review/test runs behind the verdict — a compact summary; full
  // transcripts live in the Runs tab.
  const REVIEW_ROLES = new Set(['review', 'fix-review', 'test-runner'])
  const reviewRuns = $derived(
    (task.agentRuns ?? []).filter((r) => REVIEW_ROLES.has(r.role)).reverse(),
  )
</script>

{#if task.codeReview}
  <div class="flex flex-col gap-1">
    <span class="text-sm font-medium text-surface-500">Code review <span class="text-xs font-normal italic text-surface-400">auto-generated</span></span>
    <div class="rounded-lg border border-warning-300 bg-warning-50 p-4 dark:border-warning-700 dark:bg-warning-900/20">
      <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedReview}</div>
    </div>
  </div>
{/if}

{#if reviewRuns.length > 0}
  <div class="flex flex-col gap-2">
    <span class="text-sm font-medium text-surface-500">Review &amp; test runs</span>
    <div class="flex flex-col gap-1.5">
      {#each reviewRuns as run (run.agentId)}
        <div class="flex flex-wrap items-center gap-2 rounded-md border border-surface-300 bg-surface-50 px-3 py-1.5 text-xs dark:border-surface-600 dark:bg-surface-800">
          {#if runRoleLabel(run.role)}
            <span class="rounded px-1.5 py-0.5 font-medium {runRoleClasses(run.role)}">{runRoleLabel(run.role)}</span>
          {/if}
          <span class="font-mono text-[11px] text-surface-400">{run.agentId}</span>
          <span class="rounded px-1.5 py-0.5 {runStateClasses(run.state || 'running')}">{run.state || 'running'}</span>
          {#if run.verdict}
            <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{run.verdict}</span>
          {/if}
          {#if run.costUsd > 0}
            <span class="ml-auto tabular-nums text-surface-400">{formatCostShort(run.costUsd)}</span>
          {/if}
        </div>
      {/each}
    </div>
    <span class="text-xs text-surface-400">Full transcripts are in the Runs tab.</span>
  </div>
{/if}
