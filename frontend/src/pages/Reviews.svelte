<script lang="ts">
  import { FileText, ChevronLeft } from '@lucide/svelte'
  import { renderMarkdown } from '../lib/markdown.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { commentStore } from '../stores/comments.svelte.js'
  import PlanFileView from '../components/PlanFileView.svelte'
  import { viewport } from '../lib/viewport.svelte.js'

  let { onviewtask }: { onviewtask?: (id: string) => void } = $props()

  let mobileView = $state<'list' | 'detail'>('list')

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

  const renderedCritique = $derived(renderMarkdown(selectedTask?.planCritique))

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
    mobileView = 'detail'
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

<div class="flex h-full min-h-0 overflow-hidden flex-col md:flex-row">
  <!-- Task list sidebar -->
  <div class="flex w-full shrink-0 flex-col overflow-y-auto border-r border-surface-300 bg-surface-50 dark:border-surface-700 dark:bg-surface-900 md:w-80 lg:w-72 {mobileView === 'detail' ? 'hidden md:flex' : 'flex'}">
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
              class="tap w-full rounded-lg px-4 py-3.5 text-left transition-colors
                {selectedId === t.id
                  ? 'bg-primary-100 text-primary-800 dark:bg-primary-900/40 dark:text-primary-200'
                  : 'active:bg-surface-200 md:hover:bg-surface-200 dark:active:bg-surface-800 md:dark:hover:bg-surface-800'}"
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
  <div class="flex flex-1 flex-col overflow-hidden {mobileView === 'list' ? 'hidden md:flex' : 'flex'}">
    {#if !selectedTask}
      <div class="flex flex-1 items-center justify-center text-surface-400">
        <div class="text-center">
          <FileText size={48} class="mx-auto mb-3 opacity-40" />
          <p class="text-sm">Select a task to review its {isTestPlan ? 'test plan' : 'plan'}</p>
        </div>
      </div>
    {:else}
      <!-- Header -->
      <div class="flex items-center justify-between gap-2 border-b border-surface-300 px-3 py-2.5 dark:border-surface-700 md:px-6 md:py-3">
        <div class="flex min-w-0 items-center gap-2 md:gap-3">
          {#if !viewport.isDesktop}
            <button
              type="button"
              onclick={() => (mobileView = 'list')}
              class="tap -ml-2 shrink-0 rounded-lg text-surface-600 active:bg-surface-200 dark:text-surface-300 dark:active:bg-surface-700"
              aria-label="Back to list"
            >
              <ChevronLeft size={24} />
            </button>
          {/if}
          <h2 class="truncate text-base font-semibold">{selectedTask.title}</h2>
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
        {#if selectedTask.planCritique}
          <details open class="mb-4 rounded-lg border border-warning-300 bg-warning-50 dark:border-warning-700 dark:bg-warning-900/20">
            <summary class="cursor-pointer px-4 py-2 text-sm font-semibold text-warning-800 dark:text-warning-300">
              Plan Critique (auto-review)
            </summary>
            <div class="markdown-body px-4 pb-4 text-sm text-surface-900 dark:text-surface-100">
              {@html renderedCritique}
            </div>
          </details>
        {/if}
        <div class="mb-3 flex items-center gap-2">
          <FileText size={16} class="text-surface-400" />
          <span class="text-xs font-medium text-surface-500">{isTestPlan ? 'TEST_PLAN.md' : 'PLAN.md'}</span>
          <span class="text-xs text-surface-400">— click <kbd class="rounded bg-surface-200 px-1 dark:bg-surface-700">+</kbd> on any line to comment</span>
        </div>
        <div class="rounded-lg border border-surface-300 bg-white dark:border-surface-700 dark:bg-surface-900">
          <PlanFileView taskId={selectedTask.id} planBody={selectedTask.plan || selectedTask.body} />
        </div>
      </div>

      <!-- Approve / Reject bar -->
      <div class="sticky bottom-0 border-t border-surface-300 bg-surface-50 px-3 pt-3 pb-safe dark:border-surface-700 dark:bg-surface-900 md:px-6 md:py-4">
        {#if errorMsg}
          <p class="mb-3 text-sm text-error-600 dark:text-error-400">{errorMsg}</p>
        {/if}
        <div class="flex flex-col gap-3 md:flex-row md:items-start">
          <textarea
            bind:this={feedbackRef}
            class="flex-1 resize-none rounded-lg border border-surface-300 bg-white p-3 text-base dark:border-surface-600 dark:bg-surface-800 md:p-2.5 md:text-sm"
            rows="2"
            placeholder="Rejection feedback (optional)..."
            bind:value={rejectFeedback}
          ></textarea>
          <div class="grid grid-cols-2 gap-2 md:flex md:shrink-0 md:flex-col">
            <button
              type="button"
              class="tap rounded-lg bg-success-500 px-4 py-3 text-sm font-medium text-white active:bg-success-700 disabled:opacity-50"
              onclick={approve}
              disabled={actionLoading}
            >Approve</button>
            <button
              type="button"
              class="tap rounded-lg bg-error-500 px-4 py-3 text-sm font-medium text-white active:bg-error-700 disabled:opacity-50"
              onclick={reject}
              disabled={actionLoading}
            >Reject</button>
            <button
              type="button"
              class="tap col-span-2 rounded-lg bg-primary-500 px-4 py-3 text-sm font-medium text-white active:bg-primary-700 disabled:opacity-50"
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
