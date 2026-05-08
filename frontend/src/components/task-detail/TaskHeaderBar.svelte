<script lang="ts">
  import { Copy } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import { notificationStore } from '../../stores/notifications.svelte.js'
  import { StartReview, StartFixReview } from '$lib/api'
  import { STATUS_OPTIONS, STATUS_MAP } from '../../lib/statuses.js'

  interface Props {
    task: Task
    ondelete: () => void
  }

  const { task, ondelete }: Props = $props()

  const statusOptions = STATUS_OPTIONS

  // Statuses that conflict with a running agent
  const AGENT_BLOCKED_STATUSES = new Set(['new', 'todo', 'done'])

  let statusSelectRef = $state<HTMLSelectElement | null>(null)
  let titleInputRef = $state<HTMLInputElement | null>(null)
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let deleting = $state(false)
  let copied = $state(false)
  let copiedBranch = $state(false)
  let reviewLoading = $state(false)
  let fixReviewLoading = $state(false)
  let error = $state('')

  const taskBranchName = $derived(
    task ? 'sybra/' + (task.slug ? task.slug + '-' + task.id : task.id) : '',
  )

  const hasRunningAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === task.id && a.state === 'running'),
  )

  const triaging = $derived(
    (agentStore.list ?? []).some(
      (a) => a.taskId === task.id && a.name?.startsWith('triage:') && a.state === 'running',
    ),
  )
  const evaluating = $derived(
    (agentStore.list ?? []).some(
      (a) => a.taskId === task.id && a.name?.startsWith('eval:') && a.state === 'running',
    ),
  )
  const planningAgent = $derived(
    (agentStore.list ?? []).some(
      (a) => a.taskId === task.id && a.name?.startsWith('plan:') && a.state === 'running',
    ),
  )
  const reviewingAgent = $derived(
    (agentStore.list ?? []).some(
      (a) => a.taskId === task.id && a.name?.startsWith('review:') && a.state === 'running',
    ),
  )
  const fixingReviewAgent = $derived(
    (agentStore.list ?? []).some(
      (a) => a.taskId === task.id && a.name?.startsWith('fix-review:') && a.state === 'running',
    ),
  )
  const isReviewTask = $derived(task.tags?.includes('review') ?? false)

  $effect(() => {
    if (editingTitle && titleInputRef) titleInputRef.focus()
  })

  $effect(() => {
    function onEditTitle() {
      if (!editingTitle) startEditingTitle()
    }
    function onFocusStatus() {
      statusSelectRef?.focus()
    }
    function onCopyId() {
      copyId()
    }
    function onCopyBranch() {
      copyBranch()
    }
    function onDeleteEv() {
      deleteTask()
    }
    window.addEventListener('task-detail:edit-title', onEditTitle)
    window.addEventListener('task-detail:focus-status', onFocusStatus)
    window.addEventListener('task-detail:copy-id', onCopyId)
    window.addEventListener('task-detail:copy-branch', onCopyBranch)
    window.addEventListener('task-detail:delete', onDeleteEv)
    return () => {
      window.removeEventListener('task-detail:edit-title', onEditTitle)
      window.removeEventListener('task-detail:focus-status', onFocusStatus)
      window.removeEventListener('task-detail:copy-id', onCopyId)
      window.removeEventListener('task-detail:copy-branch', onCopyBranch)
      window.removeEventListener('task-detail:delete', onDeleteEv)
    }
  })

  function startEditingTitle() {
    titleDraft = task.title
    editingTitle = true
  }

  async function saveTitle() {
    if (!titleDraft.trim() || titleDraft.trim() === task.title) {
      editingTitle = false
      return
    }
    try {
      await taskStore.update(task.id, { title: titleDraft.trim() })
    } catch (e) {
      error = String(e)
    }
    editingTitle = false
  }

  function handleTitleKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      saveTitle()
    } else if (e.key === 'Escape') {
      editingTitle = false
    }
  }

  async function updateStatus(value: string) {
    if (task.status === value) return
    try {
      await taskStore.update(task.id, { status: value })
    } catch (e) {
      error = String(e)
    }
  }

  async function updateTaskType(value: string) {
    if ((task.taskType ?? 'normal') === value) return
    try {
      await taskStore.update(task.id, { task_type: value })
    } catch (e) {
      error = String(e)
    }
  }

  async function copyId() {
    try {
      await navigator.clipboard.writeText(task.id)
      copied = true
      setTimeout(() => { copied = false }, 1500)
    } catch (e) {
      notificationStore.pushLocal('error', 'Copy failed', String(e))
    }
  }

  async function copyBranch() {
    if (!task.projectId) return
    try {
      await navigator.clipboard.writeText(taskBranchName)
      copiedBranch = true
      setTimeout(() => { copiedBranch = false }, 1500)
    } catch (e) {
      notificationStore.pushLocal('error', 'Copy failed', String(e))
    }
  }

  async function runReview() {
    reviewLoading = true
    error = ''
    try {
      await StartReview(task.id)
    } catch (e) {
      error = String(e)
    } finally {
      reviewLoading = false
    }
  }

  async function runFixReview() {
    fixReviewLoading = true
    error = ''
    try {
      await StartFixReview(task.id)
    } catch (e) {
      error = String(e)
    } finally {
      fixReviewLoading = false
    }
  }

  async function deleteTask() {
    deleting = true
    try {
      await taskStore.remove(task.id)
      ondelete()
    } catch (e) {
      error = String(e)
      deleting = false
    }
  }
</script>

{#if error}
  <p class="text-sm text-error-500">{error}</p>
{/if}

<div class="flex items-start justify-between gap-4">
  {#if editingTitle}
    <input
      bind:this={titleInputRef}
      class="text-2xl font-bold bg-transparent border-b-2 border-primary-500 outline-none w-full"
      bind:value={titleDraft}
      onblur={saveTitle}
      onkeydown={handleTitleKeydown}
    />
  {:else}
    <h1 class="text-2xl font-bold">
      <button
        type="button"
        class="cursor-pointer hover:text-primary-500 transition-colors"
        onclick={startEditingTitle}
        title="Click to edit title"
      >{task.title}</button>
    </h1>
  {/if}
  <div class="flex items-center gap-2">
    <select
      bind:this={statusSelectRef}
      data-testid="task-status-select"
      class="cursor-pointer rounded-full px-2.5 py-0.5 text-xs font-semibold transition-opacity hover:opacity-80 {STATUS_MAP[task.status]?.badgeClasses ?? 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}"
      style="appearance: auto"
      value={task.status}
      onchange={(e) => updateStatus((e.target as HTMLSelectElement).value)}
      title="Click to change status"
    >
      {#each statusOptions as s}
        <option value={s.value} disabled={hasRunningAgent && AGENT_BLOCKED_STATUSES.has(s.value)}>{s.label}</option>
      {/each}
    </select>
    <select
      data-testid="task-type-select"
      class="rounded border border-surface-300 bg-surface-100 px-2 py-0.5 text-xs font-medium dark:border-surface-600 dark:bg-surface-700"
      value={task.taskType || 'normal'}
      onchange={(e) => updateTaskType((e.target as HTMLSelectElement).value)}
      title="Task type — controls execution mode and worktree behavior"
    >
      <option value="normal">normal</option>
      <option value="debug">debug</option>
      <option value="research">research</option>
    </select>
    {#if triaging}
      <span class="inline-flex items-center gap-1 rounded-full bg-primary-200 px-2 py-0.5 text-xs font-medium text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
        Triaging
      </span>
    {/if}
    {#if planningAgent}
      <span class="inline-flex items-center gap-1 rounded-full bg-tertiary-200 px-2 py-0.5 text-xs font-medium text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
        Planning
      </span>
    {/if}
    {#if evaluating}
      <span class="inline-flex items-center gap-1 rounded-full bg-warning-200 px-2 py-0.5 text-xs font-medium text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
        Evaluating
      </span>
    {/if}
    {#if reviewingAgent}
      <span class="inline-flex items-center gap-1 rounded-full bg-warning-200 px-2 py-0.5 text-xs font-medium text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
        Reviewing
      </span>
    {/if}
    {#if task.reviewed}
      <span class="inline-flex items-center gap-1 rounded-full bg-success-200 px-2 py-0.5 text-xs font-medium text-success-800 dark:bg-success-700 dark:text-success-200" title="Review agent completed">
        ✓ Reviewed
      </span>
    {/if}
    {#if isReviewTask && task.prNumber && task.projectId}
      <button
        type="button"
        class="rounded bg-warning-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-warning-600 disabled:opacity-50"
        onclick={runReview}
        disabled={reviewLoading || reviewingAgent}
      >
        {reviewLoading ? 'Starting...' : task.reviewed ? 'Re-run Review' : 'Run Review'}
      </button>
    {/if}
    {#if task.status === 'in-review' && task.prNumber && task.projectId && !isReviewTask}
      <button
        type="button"
        class="rounded bg-tertiary-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-tertiary-600 disabled:opacity-50"
        onclick={runFixReview}
        disabled={fixReviewLoading || fixingReviewAgent}
        title="Run fix-review skill to apply unresolved PR review comments"
      >
        {fixReviewLoading ? 'Starting...' : 'Fix Review Comments'}
      </button>
    {/if}
    {#if fixingReviewAgent}
      <span class="inline-flex items-center gap-1 rounded-full bg-tertiary-200 px-2 py-0.5 text-xs font-medium text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
        Fixing review
      </span>
    {/if}
    <button
      type="button"
      class="rounded bg-surface-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-surface-600"
      onclick={copyId}
      title="Copy task ID (⌘.)"
    >
      {copied ? 'Copied!' : 'Copy ID'}
    </button>
    {#if task.projectId}
      <button
        type="button"
        class="inline-flex items-center gap-1 rounded bg-surface-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-surface-600"
        onclick={copyBranch}
        title="Copy branch name (⇧⌘.)"
      >
        <Copy size={12} />
        {copiedBranch ? 'Copied!' : 'Copy branch'}
      </button>
    {/if}
    <button
      type="button"
      class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600 disabled:opacity-50"
      onclick={deleteTask}
      disabled={deleting}
    >
      {deleting ? 'Deleting...' : 'Delete'}
    </button>
  </div>
</div>
