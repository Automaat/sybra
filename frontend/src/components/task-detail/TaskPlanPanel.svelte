<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { renderMarkdown } from '../../lib/markdown.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  // Read-only view of a task's planning artifacts, for the Plan tab of a task
  // that is past plan-review. The plan-review *decision* (approve/reject +
  // open decisions) is handled separately by PlanReviewPanel.
  const renderedPlan = $derived(renderMarkdown(task.plan))
  const renderedCritique = $derived(renderMarkdown(task.planCritique))
  const renderedResearch = $derived(renderMarkdown(task.planResearch))
  const renderedBrief = $derived(renderMarkdown(task.planBrief))
  const renderedDecisions = $derived(renderMarkdown(task.planDecisions))
</script>

{#if task.plan}
  <div class="flex flex-col gap-1">
    <span class="text-sm font-medium text-surface-500">Plan <span class="text-xs font-normal italic text-surface-400">read-only</span></span>
    <div class="rounded-lg border border-surface-300 bg-surface-100 p-4 dark:border-surface-600 dark:bg-surface-900">
      <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedPlan}</div>
    </div>
  </div>
{/if}

{#if task.planBrief}
  <div class="rounded-md border border-surface-300 bg-surface-50 p-3 dark:border-surface-700 dark:bg-surface-900">
    <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-surface-500">Final brief</div>
    <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedBrief}</div>
  </div>
{/if}

{#if task.planCritique}
  <details class="rounded-md border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-900">
    <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Plan critique</summary>
    <div class="markdown-body max-h-96 overflow-y-auto border-t border-surface-300 p-3 text-sm dark:border-surface-600">
      {@html renderedCritique}
    </div>
  </details>
{/if}

{#if task.planResearch}
  <details class="rounded-md border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-900">
    <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Plan research</summary>
    <div class="markdown-body max-h-96 overflow-y-auto border-t border-surface-300 p-3 text-sm dark:border-surface-600">
      {@html renderedResearch}
    </div>
  </details>
{/if}

{#if task.planDecisions}
  <details class="rounded-md border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-900">
    <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Decision brief</summary>
    <div class="markdown-body max-h-96 overflow-y-auto border-t border-surface-300 p-3 text-sm dark:border-surface-600">
      {@html renderedDecisions}
    </div>
  </details>
{/if}
