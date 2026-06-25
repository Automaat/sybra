<script lang="ts">
  import { CircleDot, Copy } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { notificationStore } from '../../stores/notifications.svelte.js'
  import { openLink } from '$lib/browser.svelte.js'
  import { formatDateTime } from '../../lib/dates.js'
  import AssignProjectDialog from '../AssignProjectDialog.svelte'
  import TaskTagEditor from './TaskTagEditor.svelte'
  import TaskDueDateEditor from './TaskDueDateEditor.svelte'
  import TaskMaxTurnsEditor from './TaskMaxTurnsEditor.svelte'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  let error = $state('')
  let copiedBranch = $state(false)
  let projectDialogOpen = $state(false)

  let tagEditor = $state<TaskTagEditor | null>(null)
  let dueDateEditor = $state<TaskDueDateEditor | null>(null)

  const taskBranchName = $derived(
    task ? 'sybra/' + (task.slug ? task.slug + '-' + task.id : task.id) : '',
  )

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

<div class="flex gap-6 text-sm">
  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Agent Mode</span>
    <!-- Read-only: plain text, no pill chrome, so it doesn't imply the
         click-to-edit affordance the Project/Tags pills carry. -->
    <span class="text-surface-700 dark:text-surface-300">{task.agentMode}</span>
  </div>

  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Tags</span>
    <TaskTagEditor bind:this={tagEditor} {task} onerror={handleError} />
  </div>

  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Project</span>
    <button
      type="button"
      class="w-fit rounded px-1 py-0.5 text-left transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300"
      onclick={() => (projectDialogOpen = true)}
      title="Click to change project"
    >
      {#if task.projectId}
        <span class="rounded bg-surface-200 px-2 py-0.5 font-mono dark:bg-surface-700">{task.projectId}</span>
      {:else}
        <span class="text-xs italic text-surface-400">assign project</span>
      {/if}
    </button>
  </div>

  {#if task.projectId}
    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Branch</span>
      <button
        type="button"
        class="inline-flex items-center gap-1.5 rounded bg-surface-200 px-2 py-0.5 font-mono text-left transition-colors hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
        onclick={copyBranch}
        title="Copy branch name (⇧⌘.)"
      >
        {copiedBranch ? 'Copied!' : taskBranchName}
        <Copy size={12} class="shrink-0 text-surface-400" />
      </button>
    </div>
  {/if}

  {#if task.issue}
    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Issue</span>
      <button
        type="button"
        class="flex w-fit items-center gap-1.5 text-sm text-secondary-600 hover:underline dark:text-secondary-400"
        onclick={(e) => openLink(task.issue, e)}
      >
        <CircleDot size={16} class="shrink-0" />
        {task.issue}
      </button>
    </div>
  {/if}

  {#if task.allowedTools?.length}
    <div class="flex flex-col gap-1">
      <span class="font-medium text-surface-500">Allowed Tools</span>
      <div class="flex gap-1">
        {#each task.allowedTools as tool}
          <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{tool}</span>
        {/each}
      </div>
    </div>
  {/if}

  <!-- Per-run knobs stay editable (they're the only place to set them), but
       render quietly at their default so they don't shout: Fork shows a muted
       "disabled", Max Turns a muted "global default". -->
  {#if task.agentMode === 'headless'}
  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Fork Subagents</span>
    <button
      type="button"
      class="w-fit rounded px-1 py-0.5 text-left transition-colors hover:bg-surface-200 hover:text-surface-700 dark:hover:bg-surface-700 dark:hover:text-surface-300"
      onclick={async () => {
        try {
          await taskStore.update(task.id, { fork_subagent: !task.forkSubagent })
        } catch (e) {
          error = String(e)
        }
      }}
      title="Enable CLAUDE_CODE_FORK_SUBAGENT=1 — parallel subagents, higher token cost"
    >
      {#if task.forkSubagent}
        <span class="text-primary-600 dark:text-primary-400">enabled</span>
      {:else}
        <span class="italic text-surface-400">disabled</span>
      {/if}
    </button>
  </div>
  {/if}

  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Reasoning Effort <span class="font-normal text-surface-400 text-xs">(Codex only)</span></span>
    <select
      class="w-fit rounded bg-transparent px-1 py-0.5 text-sm text-surface-600 dark:text-surface-300"
      value={task.reasoningEffort ?? ''}
      onchange={async (e) => {
        try {
          await taskStore.update(task.id, { reasoning_effort: e.currentTarget.value })
        } catch (e) {
          error = String(e)
        }
      }}
      title="Codex model_reasoning_effort. Empty = model default. Ignored for claude agents."
    >
      <option value="">default</option>
      <option value="low">low</option>
      <option value="medium">medium</option>
      <option value="high">high</option>
      <option value="xhigh">xhigh</option>
    </select>
  </div>

  <div class="flex flex-col gap-1">
    <span class="font-medium text-surface-500">Max Turns</span>
    <TaskMaxTurnsEditor {task} onerror={handleError} />
  </div>
</div>

<div class="flex flex-wrap items-center gap-4 text-xs text-surface-400">
  <span>Created: {formatDateTime(task.createdAt)}</span>
  <span>Updated: {formatDateTime(task.updatedAt)}</span>
  <div class="flex items-center gap-1">
    <span>Due:</span>
    <TaskDueDateEditor bind:this={dueDateEditor} {task} onerror={handleError} />
  </div>
</div>

<AssignProjectDialog
  open={projectDialogOpen}
  onOpenChange={(o) => (projectDialogOpen = o)}
  onassign={assignProject}
/>
