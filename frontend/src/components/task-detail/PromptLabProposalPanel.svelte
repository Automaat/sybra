<script lang="ts">
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { renderMarkdown } from '../../lib/markdown.js'
  import { isPromptLabProposal } from '../../lib/statuses.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let rejectFeedback = $state('')
  let loading = $state(false)
  let error = $state('')

  async function approve() {
    loading = true
    error = ''
    try {
      await taskStore.approveProposal(task.id)
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
      await taskStore.rejectProposal(task.id, rejectFeedback.trim())
      rejectFeedback = ''
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }
</script>

{#if isPromptLabProposal(task)}
  <div class="flex flex-col gap-3 rounded-lg border border-tertiary-300 bg-tertiary-50 p-4 dark:border-tertiary-700 dark:bg-tertiary-900/30">
    <span class="text-sm font-semibold text-tertiary-700 dark:text-tertiary-300">Prompt Lab Proposal</span>
    <p class="text-xs text-surface-600 dark:text-surface-300">
      This proposal has no concrete diff yet — it only reports rationale, evidence, and an offline
      failure-rate delta. Approving greenlights authoring the variant prompt/skill text and running
      it through the offline eval gate; it does not itself apply a change.
    </p>

    {#if error}
      <p class="text-xs text-error-500">{error}</p>
    {/if}

    {#if task.body}
      <div class="markdown-body max-h-96 overflow-y-auto rounded-md border border-tertiary-200 bg-surface-50 p-3 text-sm dark:border-tertiary-800 dark:bg-surface-900">
        {@html renderMarkdown(task.body)}
      </div>
    {/if}

    <div class="flex gap-2">
      <button
        type="button"
        class="rounded-lg bg-success-500 px-4 py-2 text-sm font-medium text-white hover:bg-success-600 disabled:opacity-50"
        onclick={approve}
        disabled={loading}
      >
        Approve authoring + offline eval
      </button>
      <button
        type="button"
        class="rounded-lg bg-error-500 px-4 py-2 text-sm font-medium text-white hover:bg-error-600 disabled:opacity-50"
        onclick={reject}
        disabled={loading}
      >
        Reject
      </button>
    </div>
    <textarea
      class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
      rows="2"
      placeholder="Rejection feedback (optional)..."
      bind:value={rejectFeedback}
      disabled={loading}
    ></textarea>
  </div>
{/if}
