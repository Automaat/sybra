<script lang="ts">
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import type { project } from '../../wailsjs/go/models.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { BOARD_COLUMNS } from '../lib/statuses.js'
  import TaskCard from '../components/TaskCard.svelte'
  import WorktreeList from '../components/WorktreeList.svelte'

  interface Props {
    projectId: string
    onback: () => void
    onviewtask: (taskId: string) => void
  }

  const { projectId, onback, onviewtask }: Props = $props()

  let p = $state<project.Project | null>(null)
  let error = $state('')
  let deleting = $state(false)
  let updatingType = $state(false)
  let activeTab = $state('tasks')

  const tabs = [
    { value: 'tasks', label: 'Tasks' },
    { value: 'worktrees', label: 'Worktrees' },
  ]

  $effect(() => {
    loadProject()
  })

  async function loadProject() {
    try {
      p = await projectStore.get(projectId)
    } catch (e) {
      error = String(e)
    }
  }

  const projectTasks = $derived(
    taskStore.list.filter((t) => t.projectId === projectId)
  )

  const tasksByColumn = $derived(
    BOARD_COLUMNS.map(col => ({
      ...col,
      tasks: col.includes.length > 0
        ? projectTasks.filter(t => col.includes.includes(t.status as any))
        : projectTasks.filter(t => t.status === col.status),
    }))
  )

  async function deleteProject() {
    if (!p) return
    deleting = true
    try {
      await projectStore.remove(projectId)
      onback()
    } catch (e) {
      error = String(e)
      deleting = false
    }
  }

  async function toggleType() {
    if (!p) return
    updatingType = true
    try {
      const newType = p.type === 'work' ? 'pet' : 'work'
      p = await projectStore.update(projectId, newType)
    } catch (e) {
      error = String(e)
    } finally {
      updatingType = false
    }
  }

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleString()
  }
</script>

<div class="flex flex-col gap-6 p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
    </svg>
    Back to projects
  </button>

  {#if error}
    <p class="text-sm text-error-500">{error}</p>
  {/if}

  {#if p}
    <div class="flex flex-col gap-6">
      <div class="flex items-start justify-between gap-4">
        <div class="flex flex-col gap-1">
          <div class="flex items-center gap-2">
            <h1 class="text-2xl font-bold">{p.owner}/{p.repo}</h1>
            {#if p.type === 'work'}
              <span class="rounded px-1.5 py-0.5 text-xs font-medium bg-warning-100 text-warning-700 dark:bg-warning-900/40 dark:text-warning-300">work</span>
            {:else}
              <span class="rounded px-1.5 py-0.5 text-xs font-medium bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400">pet</span>
            {/if}
          </div>
          <a
            href={p.url}
            target="_blank"
            rel="noopener"
            class="text-sm text-primary-500 hover:underline"
          >{p.url}</a>
        </div>
        <div class="flex items-center gap-2">
          <button
            type="button"
            class="rounded px-2.5 py-1 text-xs font-medium disabled:opacity-50 {p.type === 'work' ? 'bg-surface-200 text-surface-700 hover:bg-surface-300 dark:bg-surface-700 dark:text-surface-300 dark:hover:bg-surface-600' : 'bg-warning-100 text-warning-700 hover:bg-warning-200 dark:bg-warning-900/40 dark:text-warning-300 dark:hover:bg-warning-900/60'}"
            onclick={toggleType}
            disabled={updatingType}
          >
            {updatingType ? '...' : p.type === 'work' ? 'Switch to Pet' : 'Switch to Work'}
          </button>
          <button
            type="button"
            class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600 disabled:opacity-50"
            onclick={deleteProject}
            disabled={deleting}
          >
            {deleting ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </div>

      <div class="flex gap-6 text-sm">
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Clone Path</span>
          <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{p.clonePath}</span>
        </div>
      </div>

      <div class="flex gap-6 text-xs text-surface-400">
        <span>Created: {formatDate(p.createdAt)}</span>
        <span>Updated: {formatDate(p.updatedAt)}</span>
      </div>

      <hr class="border-surface-300 dark:border-surface-600" />

      <SegmentedControl orientation="horizontal" value={activeTab} onValueChange={(details) => (activeTab = details.value ?? 'tasks')}>
        <SegmentedControl.Control>
          <SegmentedControl.Indicator />
          {#each tabs as tab}
            <SegmentedControl.Item value={tab.value}>
              <SegmentedControl.ItemText>{tab.label}</SegmentedControl.ItemText>
              <SegmentedControl.ItemHiddenInput />
            </SegmentedControl.Item>
          {/each}
        </SegmentedControl.Control>
      </SegmentedControl>

      {#if activeTab === 'tasks'}
        <div class="flex flex-col gap-3">
          <div class="flex items-center justify-between">
            <span class="text-sm font-medium text-surface-500">Tasks ({projectTasks.length})</span>
          </div>

          {#if projectTasks.length === 0}
            <p class="py-4 text-center text-sm text-surface-400">No tasks assigned to this project</p>
          {:else}
            <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6">
              {#each tasksByColumn as col (col.status)}
                <div class="flex flex-col rounded-lg border-t-4 bg-surface-100 dark:bg-surface-900 {col.border}">
                  <div class="flex items-center justify-between px-3 py-2">
                    <h3 class="text-xs font-semibold">{col.label}</h3>
                    <span class="rounded-full bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">{col.tasks.length}</span>
                  </div>
                  <div class="flex flex-col gap-2 overflow-y-auto px-2 pb-2">
                    {#each col.tasks as t (t.id)}
                      <TaskCard task={t} onclick={() => onviewtask(t.id)} />
                    {/each}
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {:else if activeTab === 'worktrees'}
        <WorktreeList projectId={projectId} />
      {/if}
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
