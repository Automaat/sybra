<script lang="ts">
  import { MoreHorizontal } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import { notificationStore } from '../../stores/notifications.svelte.js'
  import { StartReview, StartFixReview } from '$lib/api'
  import { statusOptionsFor, STATUS_MAP, coreStatus } from '../../lib/statuses.js'
  import { isTamperFlaggedTask } from '$lib/tamper.js'

  interface Props {
    task: Task
    ondelete: () => void
  }

  const { task, ondelete }: Props = $props()

  const statusOptions = $derived(statusOptionsFor(task.status))

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
  let menuOpen = $state(false)

  function closeMenu() {
    menuOpen = false
    // Drop any lingering "Copied!" so reopening shows the default labels.
    copied = false
    copiedBranch = false
  }

  $effect(() => {
    if (!menuOpen) return
    function onKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        closeMenu()
      }
    }
    window.addEventListener('keydown', onKeydown, { capture: true })
    return () => window.removeEventListener('keydown', onKeydown, { capture: true })
  })

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
  const isTamperFlagged = $derived(isTamperFlaggedTask(task))
  const blessLoading = $derived(taskStore.isBlessing(task.id))

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

  async function blessTampering() {
    if (blessLoading || !isTamperFlagged) return
    error = ''
    try {
      await taskStore.blessTampering(task.id)
    } catch (e) {
      error = String(e)
    }
  }

  async function deleteTask() {
    if (!window.confirm(`Delete task "${task.title}"? This cannot be undone.`)) return
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
    <!--
      This control selects among the core (column) statuses, so it shows the
      rolled-up value. A granular awaiting sub-state (e.g. plan-review) still
      reads as its column name ("Planning") here; surfacing that sub-state as a
      banner under the title is tracked separately in issue #983.
    -->
    <select
      bind:this={statusSelectRef}
      data-testid="task-status-select"
      class="cursor-pointer rounded-full px-2.5 py-0.5 text-xs font-semibold transition-opacity hover:opacity-80 {STATUS_MAP[coreStatus(task.status)]?.badgeClasses ?? 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}"
      style="appearance: auto"
      value={coreStatus(task.status)}
      onchange={(e) => updateStatus((e.target as HTMLSelectElement).value)}
      title="Click to change status"
    >
      {#each statusOptions as s}
        <option value={s.value} disabled={hasRunningAgent && AGENT_BLOCKED_STATUSES.has(s.value)}>{s.label}</option>
      {/each}
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
      <!-- A passive status label, not a control: no pill/button chrome, so it
           reads as "this has been reviewed", not a toggle to click. -->
      <span class="inline-flex items-center gap-1 text-xs font-medium text-success-600 dark:text-success-400" title="The review agent has run for this task">
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
    {#if isTamperFlagged}
      <button
        type="button"
        class="rounded bg-success-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-success-700 disabled:opacity-50"
        onclick={blessTampering}
        disabled={blessLoading}
      >
        {blessLoading ? 'Blessing...' : 'Bless & send to review'}
      </button>
    {/if}
    {#if fixingReviewAgent}
      <span class="inline-flex items-center gap-1 rounded-full bg-tertiary-200 px-2 py-0.5 text-xs font-medium text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
        Fixing review
      </span>
    {/if}
    <!-- Secondary / utility actions tucked into an overflow menu. -->
    <div class="relative">
      <button
        type="button"
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        aria-label="More actions"
        title="More actions"
        class="rounded p-1 text-surface-500 transition-colors hover:bg-surface-200 hover:text-surface-800 dark:hover:bg-surface-700 dark:hover:text-surface-200"
        onclick={() => (menuOpen ? closeMenu() : (menuOpen = true))}
      >
        <MoreHorizontal size={16} />
      </button>

      {#if menuOpen}
        <button type="button" tabindex="-1" class="fixed inset-0 z-40 cursor-default" aria-label="Close menu" onclick={closeMenu}></button>
        <div role="menu" class="absolute right-0 z-50 mt-1 w-48 rounded-lg py-1 elevation-popover">
          <div class="flex flex-col gap-1 px-3 py-1.5">
            <span id="task-type-label" class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Task type</span>
            <select
              data-testid="task-type-select"
              aria-labelledby="task-type-label"
              class="rounded border border-surface-300 bg-surface-100 px-2 py-1 text-xs font-medium dark:border-surface-600 dark:bg-surface-700"
              value={task.taskType || 'normal'}
              onchange={(e) => updateTaskType((e.target as HTMLSelectElement).value)}
              title="Task type — controls execution mode and worktree behavior"
            >
              <option value="normal">normal</option>
              <option value="debug">debug</option>
              <option value="research">research</option>
            </select>
          </div>
          <div class="my-1 border-t border-surface-200 dark:border-surface-700"></div>
          <button
            type="button"
            role="menuitem"
            class="flex w-full items-center px-3 py-1.5 text-left text-xs hover:bg-surface-200 dark:hover:bg-surface-700"
            onclick={copyId}
          >
            {copied ? 'Copied!' : 'Copy ID'}
          </button>
          {#if task.projectId}
            <button
              type="button"
              role="menuitem"
              class="flex w-full items-center px-3 py-1.5 text-left text-xs hover:bg-surface-200 dark:hover:bg-surface-700"
              onclick={copyBranch}
            >
              {copiedBranch ? 'Copied!' : 'Copy branch'}
            </button>
          {/if}
          <div class="my-1 border-t border-surface-200 dark:border-surface-700"></div>
          <button
            type="button"
            role="menuitem"
            class="flex w-full items-center px-3 py-1.5 text-left text-xs text-error-600 hover:bg-error-50 disabled:opacity-50 dark:text-error-400 dark:hover:bg-error-950"
            onclick={() => { closeMenu(); deleteTask() }}
            disabled={deleting}
          >
            {deleting ? 'Deleting...' : 'Delete task'}
          </button>
        </div>
      {/if}
    </div>
  </div>
</div>
