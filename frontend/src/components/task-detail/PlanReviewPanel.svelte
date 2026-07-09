<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { renderMarkdown } from '../../lib/markdown.js'
  import { needsPlanApproval } from '../../lib/statuses.js'
  import PlanDecisionReview from './PlanDecisionReview.svelte'

  interface Props {
    task: Task
    onreviewplan?: (taskId: string) => void
  }

  const { task, onreviewplan }: Props = $props()

  let rejectFeedback = $state('')
  let loading = $state(false)
  let error = $state('')

  async function approve() {
    loading = true
    error = ''
    try {
      await taskStore.approvePlan(task.id)
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  async function reject() {
    loading = true
    error = ''
    try {
      await taskStore.rejectPlan(task.id, rejectFeedback.trim())
      rejectFeedback = ''
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  async function requestRevision(message: string) {
    rejectFeedback = message
    await reject()
  }
</script>

{#if needsPlanApproval(task)}
  <div class="flex flex-col gap-3 rounded-lg border border-tertiary-300 bg-tertiary-50 p-4 dark:border-tertiary-700 dark:bg-tertiary-900/30">
    <div class="flex items-center justify-between">
      <span class="text-sm font-semibold text-tertiary-700 dark:text-tertiary-300">Plan Review</span>
      {#if onreviewplan}
        <button
          type="button"
          class="text-xs text-primary-500 hover:underline"
          onclick={() => onreviewplan!(task.id)}
        >Review Plan →</button>
      {/if}
    </div>
    {#if error}
      <p class="text-xs text-error-500">{error}</p>
    {/if}
    <PlanDecisionReview
      {task}
      disabled={loading}
      onrequest={requestRevision}
    />
    {#if task.plan}
      <div class="markdown-body max-h-72 overflow-y-auto rounded-md border border-tertiary-200 bg-surface-50 p-3 text-sm dark:border-tertiary-800 dark:bg-surface-900">
        {@html renderMarkdown(task.plan)}
      </div>
    {/if}
    {#if task.planCritique}
      <details class="rounded-md border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-900">
        <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Plan critique</summary>
        <div class="markdown-body max-h-72 overflow-y-auto border-t border-surface-300 p-3 text-sm dark:border-surface-600">
          {@html renderMarkdown(task.planCritique)}
        </div>
      </details>
    {/if}
    <div class="flex gap-2">
      <button
        type="button"
        class="rounded-lg bg-success-500 px-4 py-2 text-sm font-medium text-white hover:bg-success-600 disabled:opacity-50"
        onclick={approve}
        disabled={loading}
      >
        Approve Plan
      </button>
      <button
        type="button"
        class="rounded-lg bg-error-500 px-4 py-2 text-sm font-medium text-white hover:bg-error-600 disabled:opacity-50"
        onclick={reject}
        disabled={loading}
      >
        Reject Plan
      </button>
    </div>
    <textarea
      class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
      rows="2"
      placeholder="Rejection feedback (optional)..."
      bind:value={rejectFeedback}
    ></textarea>
  </div>
{/if}
