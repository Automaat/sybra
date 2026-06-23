<script lang="ts">
  import { ChevronLeft } from '@lucide/svelte'
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import * as yaml from 'js-yaml'
  import type { Project, SandboxConfig } from '../../bindings/github.com/Automaat/sybra/internal/project/models.js'
  import { SetProjectSandboxConfig, SetProjectSetupCommands } from '../../bindings/github.com/Automaat/sybra/internal/sybra/projectservice.js'
  import { SetProjectWorktreeBaseRef } from '../lib/api.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { BOARD_COLUMNS } from '../lib/statuses.js'
  import { formatDateTime } from '../lib/dates.js'
  import TaskCard from '../components/TaskCard.svelte'
  import WorktreeList from '../components/WorktreeList.svelte'

  interface Props {
    projectId: string
    onback: () => void
    onviewtask: (taskId: string) => void
  }

  const { projectId, onback, onviewtask }: Props = $props()

  let p = $state<Project | null>(null)
  let error = $state('')
  let deleting = $state(false)
  let updatingType = $state(false)
  let activeTab = $state('tasks')
  let sandboxYaml = $state('')
  let sandboxSaving = $state(false)
  let sandboxError = $state('')
  let sandboxSaved = $state(false)
  let setupText = $state('')
  let setupSaving = $state(false)
  let setupError = $state('')
  let setupSaved = $state(false)
  let baseRefSaving = $state(false)
  let baseRefError = $state('')
  let baseRefSaved = $state(false)

  const tabs = [
    { value: 'tasks', label: 'Tasks' },
    { value: 'worktrees', label: 'Worktrees' },
    { value: 'setup', label: 'Setup' },
    { value: 'sandbox', label: 'Sandbox' },
  ]

  $effect(() => {
    loadProject()
  })

  $effect(() => {
    if (p) {
      sandboxYaml = p.sandbox ? yaml.dump(p.sandbox, { indent: 2 }) : ''
      setupText = (p.setupCommands ?? []).join('\n')
    }
  })

  async function loadProject() {
    try {
      p = await projectStore.get(projectId)
    } catch (e) {
      error = String(e)
    }
  }

  async function saveSetupCommands() {
    if (!p) return
    setupSaving = true
    setupError = ''
    setupSaved = false
    try {
      const cmds = setupText
        .split('\n')
        .map((l) => l.trim())
        .filter((l) => l.length > 0 && !l.startsWith('#'))
      p = await SetProjectSetupCommands(projectId, cmds)
      setupSaved = true
      setTimeout(() => { setupSaved = false }, 2000)
    } catch (e) {
      setupError = String(e)
    } finally {
      setupSaving = false
    }
  }

  async function saveWorktreeBaseRef(ref: string) {
    if (!p) return
    baseRefSaving = true
    baseRefError = ''
    baseRefSaved = false
    try {
      p = await SetProjectWorktreeBaseRef(projectId, ref)
      baseRefSaved = true
      setTimeout(() => { baseRefSaved = false }, 2000)
    } catch (e) {
      baseRefError = String(e)
    } finally {
      baseRefSaving = false
    }
  }

  async function saveSandboxConfig() {
    if (!p) return
    sandboxSaving = true
    sandboxError = ''
    sandboxSaved = false
    try {
      const parsed = sandboxYaml.trim()
        ? (yaml.load(sandboxYaml) as SandboxConfig)
        : null
      p = await SetProjectSandboxConfig(projectId, parsed as SandboxConfig)
      sandboxSaved = true
      setTimeout(() => { sandboxSaved = false }, 2000)
    } catch (e) {
      sandboxError = String(e)
    } finally {
      sandboxSaving = false
    }
  }

  const projectTasks = $derived(
    taskStore.list.filter((t) => t.projectId === projectId)
  )

  const tasksByColumn = $derived(
    BOARD_COLUMNS.map(col => ({
      ...col,
      tasks: col.includes.length > 0
        ? projectTasks.filter(t => (col.includes as string[]).includes(t.status))
        : projectTasks.filter(t => t.status === col.status),
    }))
  )

  // Tasks placed in the active board columns. Anything outside (done, cancelled,
  // or an unknown/legacy status) is excluded so a project with no on-board tasks
  // never renders as six empty buckets.
  const boardCount = $derived(tasksByColumn.reduce((n, c) => n + c.tasks.length, 0))
  const doneCount = $derived(projectTasks.filter(t => t.status === 'done').length)
  const cancelledCount = $derived(projectTasks.filter(t => t.status === 'cancelled').length)
  const otherCount = $derived(Math.max(0, projectTasks.length - boardCount - doneCount - cancelledCount))

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

</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <ChevronLeft size={16} />
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
        <span>Created: {formatDateTime(p.createdAt)}</span>
        <span>Updated: {formatDateTime(p.updatedAt)}</span>
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
          <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span class="text-sm font-medium text-surface-500">Tasks ({projectTasks.length})</span>
            {#if projectTasks.length > 0}
              <span class="text-xs text-surface-400">
                {boardCount} active · {doneCount} done{#if cancelledCount > 0} · {cancelledCount} cancelled{/if}{#if otherCount > 0} · {otherCount} other{/if}
              </span>
            {/if}
          </div>

          {#if projectTasks.length === 0}
            <p class="py-4 text-center text-sm text-surface-400">No tasks assigned to this project</p>
          {:else if boardCount === 0}
            <p class="py-4 text-center text-sm text-surface-400">
              No active tasks — {doneCount} done{#if cancelledCount > 0}, {cancelledCount} cancelled{/if}{#if otherCount > 0}, {otherCount} other{/if}.
            </p>
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
      {:else if activeTab === 'setup'}
        <div class="flex flex-col gap-3">
          <div class="flex flex-col gap-2 rounded border border-surface-300 p-3 dark:border-surface-600">
            <div class="flex items-center justify-between">
              <div class="flex flex-col gap-0.5">
                <span class="text-sm font-medium">Worktree base</span>
                <span class="text-xs text-surface-500">
                  Starting point for new worktree branches.
                  <strong>fresh</strong> = <code>origin/&lt;default&gt;</code> (always pushed state);
                  <strong>head</strong> = local HEAD (includes unpushed commits).
                </span>
              </div>
              <div class="flex items-center gap-2">
                {#if baseRefSaved}
                  <span class="text-xs text-success-500">Saved</span>
                {/if}
                {#if baseRefError}
                  <span class="text-xs text-error-500">{baseRefError}</span>
                {/if}
                <select
                  class="rounded border border-surface-300 bg-surface-50 px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-900 disabled:opacity-50"
                  value={p.worktreeBaseRef ?? 'fresh'}
                  onchange={(e) => saveWorktreeBaseRef((e.target as HTMLSelectElement).value)}
                  disabled={baseRefSaving}
                >
                  <option value="fresh">fresh</option>
                  <option value="head">head</option>
                </select>
              </div>
            </div>
          </div>

          <p class="text-sm text-surface-500">
            Machine-local shell commands run inside every newly created worktree for this project, <em>after</em>
            the <code>setup:</code> block in the repo's <code>.sybra.yaml</code>. Use this only for host-specific extras
            (e.g. copying a local <code>.env</code>); canonical toolchain bootstrap belongs in <code>.sybra.yaml</code>
            so every machine behaves identically.
          </p>
          <p class="text-xs text-surface-500">
            One command per line; lines starting with <code>#</code> are ignored. All commands share a 5-minute batch
            timeout, and a non-zero exit blocks worktree creation. Output streams to
            <code>~/.sybra/logs/worktrees/&lt;task-id&gt;-setup.log</code>.
          </p>
          <textarea
            class="h-48 w-full rounded border border-surface-300 bg-surface-50 px-3 py-2 font-mono text-sm dark:border-surface-600 dark:bg-surface-900"
            placeholder={'# one shell command per line\nmise install\nnpm ci --prefix frontend'}
            bind:value={setupText}
          ></textarea>
          {#if setupError}
            <p class="text-sm text-error-500">{setupError}</p>
          {/if}
          <div class="flex items-center gap-3">
            <button
              type="button"
              class="rounded bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              onclick={saveSetupCommands}
              disabled={setupSaving}
            >
              {setupSaving ? 'Saving...' : 'Save'}
            </button>
            {#if setupSaved}
              <span class="text-sm text-success-500">Saved</span>
            {/if}
          </div>
        </div>
      {:else if activeTab === 'sandbox'}
        <div class="flex flex-col gap-3">
          <p class="text-sm text-surface-500">
            Configure the sandbox environment for agents working on this project.
            Leave empty to disable sandbox.
          </p>
          <textarea
            class="h-64 w-full rounded border border-surface-300 bg-surface-50 px-3 py-2 font-mono text-sm dark:border-surface-600 dark:bg-surface-900"
            placeholder="# Example:&#10;image: myapp:latest&#10;port: 8080&#10;with:&#10;  - postgres:16"
            bind:value={sandboxYaml}
          ></textarea>
          {#if sandboxError}
            <p class="text-sm text-error-500">{sandboxError}</p>
          {/if}
          <div class="flex items-center gap-3">
            <button
              type="button"
              class="rounded bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              onclick={saveSandboxConfig}
              disabled={sandboxSaving}
            >
              {sandboxSaving ? 'Saving...' : 'Save'}
            </button>
            {#if sandboxSaved}
              <span class="text-sm text-success-500">Saved</span>
            {/if}
          </div>
        </div>
      {/if}
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
