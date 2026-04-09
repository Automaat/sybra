<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { commentStore } from '../stores/comments.svelte.js'
  import PlanFileView from '../components/PlanFileView.svelte'

  let { onviewtask }: { onviewtask?: (id: string) => void } = $props()

  let selectedId = $state<string | null>(null)
  let rejectFeedback = $state('')
  let actionLoading = $state(false)
  let feedbackRef = $state<HTMLTextAreaElement | null>(null)
  let errorMsg = $state('')
  let hasLiveAgent = $state(false)

  const allReviewTasks = $derived([
    ...taskStore.byStatus('plan-review'),
    ...taskStore.byStatus('test-plan-review'),
  ])

  const selectedTask = $derived(
    selectedId ? taskStore.items.get(selectedId) ?? null : null,
  )

  const isTestPlan = $derived(selectedTask?.status === 'test-plan-review')

  $effect(() => {
    if (selectedId) {
      commentStore.load(selectedId)
      void refreshLiveAgent(selectedId)
    }
  })

  async function refreshLiveAgent(id: string) {
    const task = taskStore.items.get(id)
    try {
      if (task?.status === 'test-plan-review') {
        hasLiveAgent = await taskStore.hasLiveTestPlanAgent(id)
      } else {
        hasLiveAgent = await taskStore.hasLivePlanAgent(id)
      }
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

  async function approve() {
    if (!selectedId) return
    actionLoading = true
    errorMsg = ''
    try {
      if (isTestPlan) {
        await taskStore.approveTestPlan(selectedId)
      } else {
        await taskStore.approvePlan(selectedId)
      }
      selectedId = null
    } catch (e) {
      errorMsg = String(e)
    } finally {
      actionLoading = false
    }
  }

  async function reject() {
    if (!selectedId) return
    actionLoading = true
    errorMsg = ''
    try {
      if (isTestPlan) {
        await taskStore.rejectTestPlan(selectedId, rejectFeedback)
      } else {
        await taskStore.rejectPlan(selectedId, rejectFeedback)
      }
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
      if (isTestPlan) {
        await taskStore.sendTestPlanMessage(selectedId, rejectFeedback)
      } else {
        await taskStore.sendPlanMessage(selectedId, rejectFeedback)
      }
      rejectFeedback = ''
    } catch (e) {
      errorMsg = String(e)
    } finally {
      actionLoading = false
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    const target = e.target as HTMLElement
    if (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable) return
    if (e.metaKey || e.ctrlKey || e.altKey) return

    if (e.key === 'a' && selectedId && !actionLoading) {
      e.preventDefault()
      void approve()
      return
    }

    if (e.key === 'r' && selectedId && !actionLoading) {
      e.preventDefault()
      void reject()
      return
    }

    if (e.key === 'j' || e.key === 'ArrowDown') {
      e.preventDefault()
      const tasks = allReviewTasks
      if (tasks.length === 0) return
      const idx = selectedId ? tasks.findIndex(t => t.id === selectedId) : -1
      const next = tasks[Math.min(idx + 1, tasks.length - 1)]
      if (next) void selectTask(next.id)
      return
    }

    if (e.key === 'k' || e.key === 'ArrowUp') {
      e.preventDefault()
      const tasks = allReviewTasks
      if (tasks.length === 0) return
      const idx = selectedId ? tasks.findIndex(t => t.id === selectedId) : tasks.length
      const prev = tasks[Math.max(idx - 1, 0)]
      if (prev) void selectTask(prev.id)
      return
    }

    if (e.key === 'c' && selectedId) {
      e.preventDefault()
      feedbackRef?.focus()
      return
    }
  }

  $effect(() => {
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })
</script>

<div class="flex h-full overflow-hidden">
  <!-- Task list sidebar -->
  <div class="flex w-72 shrink-0 flex-col overflow-y-auto border-r border-surface-300 bg-surface-50 dark:border-surface-700 dark:bg-surface-900">
    <div class="border-b border-surface-300 px-4 py-3 dark:border-surface-700">
      <h3 class="text-sm font-semibold text-surface-700 dark:text-surface-300">
        Reviews
        {#if allReviewTasks.length > 0}
          <span class="ml-1.5 rounded-full bg-warning-500 px-1.5 py-0.5 text-xs font-medium text-white">{allReviewTasks.length}</span>
        {/if}
      </h3>
    </div>

    {#if allReviewTasks.length === 0}
      <div class="p-4 text-sm text-surface-400 italic">No tasks pending review</div>
    {:else}
      <ul class="flex flex-col gap-1 p-2">
        {#each allReviewTasks as t}
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
                <div class="mt-0.5 flex shrink-0 items-center gap-1">
                  {#if t.status === 'test-plan-review'}
                    <span class="rounded bg-secondary-100 px-1.5 py-0.5 text-xs font-medium text-secondary-700 dark:bg-secondary-900/40 dark:text-secondary-300">Test</span>
                  {:else}
                    <span class="rounded bg-warning-100 px-1.5 py-0.5 text-xs font-medium text-warning-700 dark:bg-warning-900/40 dark:text-warning-300">Plan</span>
                  {/if}
                  {#if unresolvedCount > 0}
                    <span class="rounded-full bg-warning-500 px-1.5 py-0.5 text-xs font-medium text-white">{unresolvedCount}</span>
                  {/if}
                </div>
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
          <p class="text-sm">Select a task to review its {isTestPlan ? 'test plan' : 'plan'}</p>
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
          <span class="text-xs font-medium text-surface-500">{isTestPlan ? 'TEST_PLAN.md' : 'PLAN.md'}</span>
          <span class="text-xs text-surface-400">— click <kbd class="rounded bg-surface-200 px-1 dark:bg-surface-700">+</kbd> on any line to comment</span>
        </div>
        <div class="rounded-lg border border-surface-300 bg-white dark:border-surface-700 dark:bg-surface-900">
          <PlanFileView taskId={selectedTask.id} planBody={isTestPlan ? selectedTask.body : (selectedTask.plan || selectedTask.body)} />
        </div>
      </div>

      <!-- Approve / Reject bar -->
      <div class="border-t border-surface-300 bg-surface-50 px-6 py-4 dark:border-surface-700 dark:bg-surface-900">
        {#if errorMsg}
          <p class="mb-3 text-sm text-error-600 dark:text-error-400">{errorMsg}</p>
        {/if}
        <div class="flex items-start gap-3">
          <textarea
            bind:this={feedbackRef}
            class="flex-1 resize-none rounded-lg border border-surface-300 bg-white p-2.5 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="2"
            placeholder="Rejection feedback (optional) — unresolved comments are included automatically..."
            bind:value={rejectFeedback}
          ></textarea>
          <div class="flex shrink-0 flex-col gap-2">
            <button
              type="button"
              class="rounded-lg bg-success-500 px-4 py-2 text-sm font-medium text-white hover:bg-success-600 disabled:opacity-50"
              onclick={approve}
              disabled={actionLoading}
            >Approve {isTestPlan ? 'Test Plan' : 'Plan'}</button>
            <button
              type="button"
              class="rounded-lg bg-error-500 px-4 py-2 text-sm font-medium text-white hover:bg-error-600 disabled:opacity-50"
              onclick={reject}
              disabled={actionLoading}
            >Reject {isTestPlan ? 'Test Plan' : 'Plan'}</button>
            <button
              type="button"
              class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              onclick={sendMessage}
              disabled={actionLoading || !rejectFeedback.trim() || !hasLiveAgent}
              title={hasLiveAgent ? 'Send message to live agent' : 'No live agent'}
            >Send Message</button>
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>
