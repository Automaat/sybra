<script lang="ts">
  import { CircleDot, Copy, FolderGit2, GitBranch, Bot, Tag, Calendar, Clock, CalendarClock, Wrench, Hash, DollarSign } from '@lucide/svelte'
  import { onMount } from 'svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { notificationStore } from '../../stores/notifications.svelte.js'
  import { openLink } from '$lib/browser.svelte.js'
  import { formatDateTime, formatShortDate, timeAgo } from '../../lib/dates.js'
  import { taskTotalCost, taskRunCount, formatCost } from '../../lib/cost.js'
  import { fallbackReasoningEffortOptions, loadReasoningEffortOptions } from '../../lib/codex-reasoning.js'
  import AssignProjectDialog from '../AssignProjectDialog.svelte'
  import TaskTagEditor from './TaskTagEditor.svelte'
  import TaskDueDateEditor from './TaskDueDateEditor.svelte'
  import TaskMaxTurnsEditor from './TaskMaxTurnsEditor.svelte'
  import TaskNodeAssignment from './TaskNodeAssignment.svelte'
  import { clusterStore } from '../../stores/cluster.svelte.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let error = $state('')
  let copiedId = $state(false)
  let copiedBranch = $state(false)
  let projectDialogOpen = $state(false)

  let tagEditor = $state<TaskTagEditor | null>(null)
  let dueDateEditor = $state<TaskDueDateEditor | null>(null)
  let reasoningEffortOptions = $state(fallbackReasoningEffortOptions)

  onMount(() => {
    loadReasoningEffortOptions().then((options) => {
      reasoningEffortOptions = options
    })
  })

  const taskBranchName = $derived(
    task ? 'sybra/' + (task.slug ? task.slug + '-' + task.id : task.id) : '',
  )

  const totalCost = $derived(taskTotalCost(task))
  const runCount = $derived(taskRunCount(task))

  $effect(() => {
    function onEditTags() { tagEditor?.start() }
    function onOpenDueDate() { dueDateEditor?.start() }
    window.addEventListener('task-detail:edit-tags', onEditTags)
    window.addEventListener('open-due-date', onOpenDueDate)
    return () => {
      window.removeEventListener('task-detail:edit-tags', onEditTags)
      window.removeEventListener('open-due-date', onOpenDueDate)
    }
  })

  async function assignProject(projectId: string) {
    if ((task.projectId ?? '') === projectId) return
    try {
      await taskStore.update(task.id, { project_id: projectId })
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function copyId() {
    try {
      await navigator.clipboard.writeText(task.id)
      copiedId = true
      setTimeout(() => { copiedId = false }, 1500)
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

  function handleError(msg: string) { error = msg }
</script>

{#if error}
  <p class="text-xs text-error-500">{error}</p>
{/if}

<!-- Metadata as an aligned properties list: a quiet label column + a single
     value column with leading type icons, so the eye scans one edge. The row
     order runs primary → secondary (identity, then timestamps). -->
<dl class="grid grid-cols-[auto_1fr] items-center gap-x-5 gap-y-2.5 text-sm">
  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Task ID</dt>
  <dd>
    <button
      type="button"
      class="-mx-1.5 inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-left font-mono text-surface-700 transition-colors hover:bg-surface-200 dark:text-surface-300 dark:hover:bg-surface-700"
      onclick={copyId}
      title="Copy task ID (⌘.)"
    >
      <Hash size={13} class="shrink-0 text-surface-400" />
      {copiedId ? 'Copied!' : task.id}
      <Copy size={12} class="shrink-0 text-surface-400" />
    </button>
  </dd>

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Project</dt>
  <dd>
    <button
      type="button"
      class="-mx-1.5 inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-left transition-colors hover:bg-surface-200 dark:hover:bg-surface-700"
      onclick={() => (projectDialogOpen = true)}
      title="Click to change project"
    >
      <FolderGit2 size={13} class="shrink-0 text-surface-400" />
      {#if task.projectId}
        <span class="font-mono text-surface-700 dark:text-surface-300">{task.projectId}</span>
      {:else}
        <span class="text-xs italic text-surface-400">assign project</span>
      {/if}
    </button>
  </dd>

  {#if clusterStore.enabled}
    <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Node</dt>
    <dd>
      <TaskNodeAssignment {task} />
    </dd>
  {/if}

  {#if task.projectId}
    <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Branch</dt>
    <dd>
      <button
        type="button"
        class="-mx-1.5 inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-left font-mono text-surface-700 transition-colors hover:bg-surface-200 dark:text-surface-300 dark:hover:bg-surface-700"
        onclick={copyBranch}
        title="Copy branch name (⇧⌘.)"
      >
        <GitBranch size={13} class="shrink-0 text-surface-400" />
        {copiedBranch ? 'Copied!' : taskBranchName}
        <Copy size={12} class="shrink-0 text-surface-400" />
      </button>
    </dd>
  {/if}

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Cost</dt>
  <dd class="flex items-center gap-1.5 text-surface-700 dark:text-surface-300">
    <DollarSign size={13} class="shrink-0 text-surface-400" />
    {formatCost(totalCost)} · {runCount} {runCount === 1 ? 'run' : 'runs'}
  </dd>

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Mode</dt>
  <dd class="flex items-center gap-1.5 text-surface-700 dark:text-surface-300">
    <Bot size={13} class="shrink-0 text-surface-400" />
    {task.agentMode}
  </dd>

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Tags</dt>
  <dd class="flex items-center gap-1.5">
    <Tag size={13} class="shrink-0 text-surface-400" />
    <TaskTagEditor bind:this={tagEditor} {task} onerror={handleError} />
  </dd>

  {#if task.issue}
    <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Issue</dt>
    <dd>
      <button
        type="button"
        class="flex w-fit items-center gap-1.5 text-secondary-600 hover:underline dark:text-secondary-400"
        onclick={(e) => openLink(task.issue, e)}
      >
        <CircleDot size={13} class="shrink-0" />
        {task.issue}
      </button>
    </dd>
  {/if}

  {#if task.allowedTools?.length}
    <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Tools</dt>
    <dd class="flex items-center gap-1.5">
      <Wrench size={13} class="shrink-0 text-surface-400" />
      <div class="flex flex-wrap gap-1">
        {#each task.allowedTools as tool}
          <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{tool}</span>
        {/each}
      </div>
    </dd>
  {/if}

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Created</dt>
  <dd class="flex items-center gap-1.5 text-surface-500" title={formatDateTime(task.createdAt)}>
    <Calendar size={13} class="shrink-0 text-surface-400" />
    {formatShortDate(task.createdAt)}
  </dd>

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Updated</dt>
  <dd class="flex items-center gap-1.5 text-surface-500" title={formatDateTime(task.updatedAt)}>
    <Clock size={13} class="shrink-0 text-surface-400" />
    {timeAgo(task.updatedAt)}
  </dd>

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Due</dt>
  <dd class="flex items-center gap-1.5">
    <CalendarClock size={13} class="shrink-0 text-surface-400" />
    <TaskDueDateEditor bind:this={dueDateEditor} {task} onerror={handleError} />
  </dd>

  <!-- Per-run execution knobs, shown inline with the rest of the properties. -->
  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Reasoning Effort</dt>
  <dd>
    <select
      aria-label="Reasoning Effort"
      class="-mx-1 w-fit rounded bg-transparent px-1 py-0.5 text-sm text-surface-600 dark:text-surface-300"
      value={task.reasoningEffort ?? ''}
      onchange={async (e) => {
        try {
          await taskStore.update(task.id, { reasoning_effort: e.currentTarget.value })
        } catch (e) {
          error = String(e)
        }
      }}
      title="Provider reasoning effort. Empty = model default."
    >
      <option value="">default</option>
      {#each reasoningEffortOptions as option}
        <option value={option.value}>{option.label}</option>
      {/each}
    </select>
  </dd>

  <dt class="text-[11px] font-medium uppercase tracking-wide text-surface-400">Max Turns</dt>
  <dd><TaskMaxTurnsEditor {task} onerror={handleError} /></dd>
</dl>

<AssignProjectDialog
  open={projectDialogOpen}
  onOpenChange={(o) => (projectDialogOpen = o)}
  onassign={assignProject}
/>
