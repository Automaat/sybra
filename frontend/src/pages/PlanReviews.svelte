<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { commentStore } from '../stores/comments.svelte.js'
  import PlanFileView from '../components/PlanFileView.svelte'

  let { onviewtask }: { onviewtask?: (id: string) => void } = $props()

  let selectedId = $state<string | null>(null)
  let rejectFeedback = $state('')
  let actionLoading = $state(false)
  let errorMsg = $state('')
  let hasLiveAgent = $state(false)

  const planReviewTasks = $derived(taskStore.byStatus('plan-review'))

  const selectedTask = $derived(
    selectedId ? taskStore.items.get(selectedId) ?? null : null,
  )

  $effect(() => {
    if (selectedId) {
      commentStore.load(selectedId)
      void refreshLiveAgent(selectedId)
    }
  })

  async function refreshLiveAgent(id: string) {
    try {
      hasLiveAgent = await taskStore.hasLivePlanAgent(id)
    } catch {
      hasLiveAgent = false
    }
  }

  async function selectTask(id: string) {
    selectedId = id
    rejectFeedback = ''
    errorMsg = ''
    await commentStore.load(id)
    await refreshLiveAgent(id)
  }

  async function approvePlan() {
    if (!selectedId) return
    actionLoading = true
    errorMsg = ''
    try {
      await taskStore.approvePlan(selectedId)
      selectedId = null
    } catch (e) {
      errorMsg = String(e)
    } finally {
      actionLoading = false
    }
  }

  async function rejectPlan() {
    if (!selectedId) return
    actionLoading = true
    errorMsg = ''
    try {
      await taskStore.rejectPlan(selectedId, rejectFeedback)
      rejectFeedback = ''
      selectedId = null
    } catch (e) {
      errorMsg = String(e)
    } finally {
      actionLoading = false
    }
  }

  async function sendMessage() {
    if (!selectedId || !rejectFeedback.trim()) return
    actionLoading = true
    errorMsg = ''
    try {
      await taskStore.sendPlanMessage(selectedId, rejectFeedback)
      rejectFeedback = ''
    } catch (e) {
      errorMsg = String(e)
    } finally {
      actionLoading = false
    }
  }
</script>

<div class="flex h-full overflow-hidden">
  <!-- Task list sidebar -->
  <div class="flex w-72 shrink-0 flex-col overflow-y-auto border-r border-surface-300 bg-surface-50 dark:border-surface-700 dark:bg-surface-900">
    <div class="border-b border-surface-300 px-4 py-3 dark:border-surface-700">
      <h3 class="text-sm font-semibold text-surface-700 dark:text-surface-300">
        Plan Review
        {#if planReviewTasks.length > 0}
          <span class="ml-1.5 rounded-full bg-warning-500 px-1.5 py-0.5 text-xs font-medium text-white">{planReviewTasks.length}</span>
        {/if}
      </h3>
    </div>

    {#if planReviewTasks.length === 0}
      <div class="p-4 text-sm text-surface-400 italic">No tasks pending plan review</div>
    {:else}
      <ul class="flex flex-col gap-1 p-2">
        {#each planReviewTasks as t}
          {@const unresolvedCount = commentStore.unresolvedCount(t.id)}
          <li>
            <button
              type="button"
              class="w-full rounded-lg px-3 py-2.5 text-left transition-colors
                {selectedId === t.id
                  ? 'bg-primary-100 text-primary-800 dark:bg-primary-900/40 dark:text-primary-200'
                  : 'hover:bg-surface-200 dark:hover:bg-surface-800'}"
              onclick={() => selectTask(t.id)}
            >
              <div class="flex items-start justify-between gap-2">
                <span class="text-sm font-medium leading-snug">{t.title}</span>
                {#if unresolvedCount > 0}
                  <span class="mt-0.5 shrink-0 rounded-full bg-warning-500 px-1.5 py-0.5 text-xs font-medium text-white">{unresolvedCount}</span>
                {/if}
              </div>
              {#if t.projectId}
                <div class="mt-1 text-xs text-surface-400">{t.projectId}</div>
              {/if}
              {#if t.tags?.length > 0}
                <div class="mt-1 flex flex-wrap gap-1">
                  {#each t.tags as tag}
                    <span class="rounded bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">{tag}</span>
                  {/each}
                </div>
              {/if}
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <!-- Review panel -->
  <div class="flex flex-1 flex-col overflow-hidden">
    {#if !selectedTask}
      <div class="flex flex-1 items-center justify-center text-surface-400">
        <div class="text-center">
          <svg class="mx-auto mb-3 h-12 w-12 opacity-40" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z" />
          </svg>
          <p class="text-sm">Select a task to review its plan</p>
        </div>
      </div>
    {:else}
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-surface-300 px-6 py-3 dark:border-surface-700">
        <div class="flex items-center gap-3">
          <h2 class="text-base font-semibold">{selectedTask.title}</h2>
          {#if onviewtask}
            <button
              type="button"
              class="text-xs text-primary-500 hover:underline"
              onclick={() => onviewtask!(selectedTask!.id)}
            >View Task →</button>
          {/if}
        </div>
        {#if commentStore.unresolvedCount(selectedTask.id) > 0}
          <span class="rounded-full bg-warning-100 px-3 py-1 text-xs font-medium text-warning-700 dark:bg-warning-900/30 dark:text-warning-400">
            {commentStore.unresolvedCount(selectedTask.id)} unresolved {commentStore.unresolvedCount(selectedTask.id) === 1 ? 'comment' : 'comments'}
          </span>
        {/if}
      </div>

      <!-- Plan content -->
      <div class="flex-1 overflow-y-auto px-6 py-4">
        <div class="mb-3 flex items-center gap-2">
          <svg class="h-4 w-4 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z" />
          </svg>
          <span class="text-xs font-medium text-surface-500">PLAN.md</span>
          <span class="text-xs text-surface-400">— click <kbd class="rounded bg-surface-200 px-1 dark:bg-surface-700">+</kbd> on any line to comment</span>
        </div>
        <div class="rounded-lg border border-surface-300 bg-white dark:border-surface-700 dark:bg-surface-900">
          <PlanFileView taskId={selectedTask.id} planBody={selectedTask.body} />
        </div>
      </div>

      <!-- Approve / Reject bar -->
      <div class="border-t border-surface-300 bg-surface-50 px-6 py-4 dark:border-surface-700 dark:bg-surface-900">
        {#if errorMsg}
          <p class="mb-3 text-sm text-error-600 dark:text-error-400">{errorMsg}</p>
        {/if}
        <div class="flex items-start gap-3">
          <textarea
            class="flex-1 resize-none rounded-lg border border-surface-300 bg-white p-2.5 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="2"
            placeholder="Rejection feedback (optional) — unresolved comments are included automatically..."
            bind:value={rejectFeedback}
          ></textarea>
          <div class="flex shrink-0 flex-col gap-2">
            <button
              type="button"
              class="rounded-lg bg-success-500 px-4 py-2 text-sm font-medium text-white hover:bg-success-600 disabled:opacity-50"
              onclick={approvePlan}
              disabled={actionLoading}
            >Approve Plan</button>
            <button
              type="button"
              class="rounded-lg bg-error-500 px-4 py-2 text-sm font-medium text-white hover:bg-error-600 disabled:opacity-50"
              onclick={rejectPlan}
              disabled={actionLoading}
            >Reject Plan</button>
            <button
              type="button"
              class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              onclick={sendMessage}
              disabled={actionLoading || !rejectFeedback.trim() || !hasLiveAgent}
              title={hasLiveAgent ? 'Send message to live plan agent' : 'No live plan agent'}
            >Send Message</button>
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>
