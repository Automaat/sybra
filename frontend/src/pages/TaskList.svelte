<script lang="ts">
  import { Search, Filter } from '@lucide/svelte'
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { BOARD_COLUMNS, type BoardColumn } from '../lib/statuses.js'
  import { navStore } from '../lib/navigation.svelte.js'
  import { matchesQuery, matchesProject, matchesTags, matchesAgentMode } from '../lib/task-filters.js'
  import TaskTimeline from '../components/TaskTimeline.svelte'
  import StatusPicker from '../components/StatusPicker.svelte'
  import PriorityPicker from '../components/PriorityPicker.svelte'
  import AssignProjectDialog from '../components/AssignProjectDialog.svelte'
  import TaskListFilterBar from '../components/TaskListFilterBar.svelte'
  import TaskListView from '../components/TaskListView.svelte'
  import TaskBoardView from '../components/TaskBoardView.svelte'
  import MobileSheet from '../components/shell/MobileSheet.svelte'
  import { viewModeStore } from '../lib/view-mode.svelte.js'
  import {
    handleTaskListKeydown,
    type TaskListKeyAction,
  } from '../lib/task-list-keyboard.js'

  interface Props {
    onselect: (id: string) => void
    filter?: 'in-progress'
    onnewTask?: () => void
    onfocusedtaskchange?: (taskId: string | null) => void
  }

  const { onselect, filter, onnewTask, onfocusedtaskchange }: Props = $props()

  let filtersOpen = $state(false)
  let collapsedColumns = $state<Set<string>>(new Set(['testing', 'done']))

  let timelineRef = $state<TaskTimeline | null>(null)

  const viewMode = $derived(viewModeStore.mode)

  function toggleColumn(status: string) {
    const next = new Set(collapsedColumns)
    if (next.has(status)) next.delete(status)
    else next.add(status)
    collapsedColumns = next
  }

  async function moveTask(taskId: string, status: string) {
    const existing = taskStore.tasks.get(taskId)
    if (!existing || existing.status === status) return
    try {
      await taskStore.update(taskId, { status })
    } catch (err) {
      notificationStore.pushLocal('error', 'Move failed', String(err))
    }
  }

  // Board keyboard navigation state
  let focusedColIdx = $state(-1)
  let focusedRowIdx = $state(-1)

  // Picker state
  let statusPickerOpen = $state(false)
  let priorityPickerOpen = $state(false)
  let assignProjectOpen = $state(false)

  function columnTasks(col: BoardColumn): Task[] {
    const statuses = col.includes.length > 0 ? col.includes : [col.status]
    return filteredByStatuses(statuses)
  }

  function getColumnTasksByIdx(colIndex: number): Task[] {
    const col = visibleColumns[colIndex]
    if (!col) return []
    return columnTasks(col)
  }

  const focusedTaskId = $derived.by((): string | null => {
    if (focusedColIdx < 0 || focusedRowIdx < 0) return null
    const tasks = viewMode === 'list' || viewMode === 'timeline'
      ? allFilteredTasks
      : getColumnTasksByIdx(focusedColIdx)
    return tasks[focusedRowIdx]?.id ?? null
  })

  const focusedTask = $derived(focusedTaskId ? taskStore.tasks.get(focusedTaskId) ?? null : null)

  $effect(() => {
    onfocusedtaskchange?.(focusedTaskId)
  })

  function scrollFocusedIntoView(): void {
    requestAnimationFrame(() => {
      document.querySelector('[data-focused-task]')?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
    })
  }

  async function changeStatusOfFocused(status: string) {
    if (!focusedTaskId) return
    try {
      await taskStore.update(focusedTaskId, { status })
    } catch (err) {
      notificationStore.pushLocal('error', 'Status change failed', String(err))
    }
    statusPickerOpen = false
  }

  async function changePriorityOfFocused(priority: string) {
    if (!focusedTaskId) return
    try {
      await taskStore.update(focusedTaskId, { priority })
    } catch (err) {
      notificationStore.pushLocal('error', 'Priority change failed', String(err))
    }
    priorityPickerOpen = false
  }

  async function assignProjectToFocused(projectId: string) {
    if (!focusedTaskId) return
    try {
      await taskStore.update(focusedTaskId, { project_id: projectId })
    } catch (err) {
      notificationStore.pushLocal('error', 'Project assignment failed', String(err))
    }
    assignProjectOpen = false
  }

  function applyAction(a: TaskListKeyAction) {
    switch (a.type) {
      case 'set-focus':
        focusedColIdx = a.colIdx
        focusedRowIdx = a.rowIdx
        if (a.scroll) scrollFocusedIntoView()
        break
      case 'clear-focus':
        focusedColIdx = -1
        focusedRowIdx = -1
        break
      case 'select-focused':
        if (focusedTaskId) onselect(focusedTaskId)
        break
      case 'open-due-date-focused':
        if (focusedTaskId) {
          onselect(focusedTaskId)
          requestAnimationFrame(() => window.dispatchEvent(new CustomEvent('open-due-date')))
        }
        break
      case 'focus-search':
        window.dispatchEvent(new CustomEvent('focus-search'))
        break
      case 'open-picker':
        if (a.kind === 'status') statusPickerOpen = true
        else if (a.kind === 'priority') priorityPickerOpen = true
        else assignProjectOpen = true
        break
      case 'new-task':
        onnewTask?.()
        break
      case 'timeline-zoom':
        if (a.dir === 'in') timelineRef?.cycleZoomIn()
        else timelineRef?.cycleZoomOut()
        break
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    const lengths = visibleColumns.map((c) => columnTasks(c).length)
    const result = handleTaskListKeydown(e, {
      viewMode,
      focusedColIdx,
      focusedRowIdx,
      focusedTaskId,
      allFilteredTasksLength: allFilteredTasks.length,
      columnTasksLengths: lengths,
      anyPickerOpen: statusPickerOpen || priorityPickerOpen || assignProjectOpen,
    })
    if (result.preventDefault) e.preventDefault()
    if (result.action) applyAction(result.action)
  }

  $effect(() => {
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })

  // Listen for toggle-view event from App.svelte (Cmd+B)
  $effect(() => {
    function onToggleView() {
      viewModeStore.cycle()
      focusedColIdx = -1
      focusedRowIdx = -1
    }
    window.addEventListener('toggle-view', onToggleView)
    return () => window.removeEventListener('toggle-view', onToggleView)
  })

  $effect(() => {
    if (filter !== 'in-progress') return
    requestAnimationFrame(() => {
      document.querySelector('[data-col-status="in-progress"]')?.scrollIntoView({
        behavior: 'smooth',
        block: 'nearest',
        inline: 'start',
      })
      const idx = BOARD_COLUMNS.findIndex((c) => c.status === 'in-progress')
      if (idx >= 0) focusedColIdx = idx
    })
  })

  // Filter state
  let searchQuery = $state('')
  let selectedProjectId = $state('')
  let selectedTags = $state<string[]>([])
  let selectedAgentMode = $state('')
  let showDone = $state(false)
  // Derived: unique tags across all tasks
  const allTags = $derived(
    [...new Set(taskStore.list.flatMap((t: Task) => t.tags ?? []))].sort()
  )

  function filteredByStatuses(statuses: string[]): Task[] {
    return taskStore.list.filter((t: Task) => {
      if (!statuses.includes(t.status)) return false
      if (!matchesQuery(t, searchQuery)) return false
      if (!matchesProject(t, selectedProjectId)) return false
      if (!matchesTags(t, selectedTags)) return false
      if (!matchesAgentMode(t, selectedAgentMode)) return false
      return true
    })
  }

  const statusOrder: Record<string, number> = {
    'new': 0, 'todo': 1, 'planning': 2, 'plan-review': 3,
    'in-progress': 4, 'in-review': 5, 'testing': 6, 'test-plan-review': 7,
    'human-required': 8, 'done': 9,
  }
  const priorityOrder: Record<string, number> = {
    'urgent': 0, 'high': 1, 'medium': 2, 'low': 3, '': 4,
  }

  const allFilteredTasks = $derived.by(() => {
    return taskStore.list.filter((t: Task) => {
      if (!showDone && (t.status === 'done' || t.status === 'cancelled')) return false
      if (!matchesQuery(t, searchQuery)) return false
      if (!matchesProject(t, selectedProjectId)) return false
      if (!matchesTags(t, selectedTags)) return false
      if (!matchesAgentMode(t, selectedAgentMode)) return false
      return true
    }).sort((a, b) => {
      const statusDiff = (statusOrder[a.status] ?? 99) - (statusOrder[b.status] ?? 99)
      if (statusDiff !== 0) return statusDiff
      return (priorityOrder[a.priority ?? ''] ?? 4) - (priorityOrder[b.priority ?? ''] ?? 4)
    })
  })

  const visibleColumns = $derived(
    showDone ? BOARD_COLUMNS : BOARD_COLUMNS.filter(c => c.status !== 'done')
  )

  const hasActiveFilters = $derived(
    Boolean(searchQuery) || Boolean(selectedProjectId) || selectedTags.length > 0 || Boolean(selectedAgentMode)
  )

  function clearFilters() {
    searchQuery = ''
    selectedProjectId = ''
    selectedTags = []
    selectedAgentMode = ''
  }

  function addTag(tag: string) {
    if (!selectedTags.includes(tag)) {
      selectedTags = [...selectedTags, tag]
    }
  }

  function removeTag(tag: string) {
    selectedTags = selectedTags.filter((t) => t !== tag)
  }

  const agentModes = [
    { value: '', label: 'All' },
    { value: 'headless', label: 'Headless' },
    { value: 'interactive', label: 'Interactive' },
  ]
</script>

{#if statusPickerOpen && focusedTask}
  <StatusPicker
    currentStatus={focusedTask.status}
    onpick={changeStatusOfFocused}
    onclose={() => (statusPickerOpen = false)}
  />
{/if}

{#if priorityPickerOpen && focusedTask}
  <PriorityPicker
    currentPriority={focusedTask.priority ?? ''}
    onpick={changePriorityOfFocused}
    onclose={() => (priorityPickerOpen = false)}
  />
{/if}

<AssignProjectDialog
  open={assignProjectOpen}
  onOpenChange={(o) => (assignProjectOpen = o)}
  onassign={assignProjectToFocused}
/>

<div class="flex h-full min-h-0 flex-col">
  <!-- Mobile filter trigger -->
  <div class="flex shrink-0 items-center gap-2 border-b border-surface-200 px-3 py-2 dark:border-surface-800 md:hidden">
    <div class="relative flex-1">
      <Search size={16} class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-surface-400" />
      <input
        type="text"
        bind:value={searchQuery}
        placeholder="Search tasks..."
        class="h-10 w-full rounded-md border border-surface-300 bg-surface-50 pl-9 pr-2 text-base outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-700 dark:bg-surface-800 dark:focus:border-primary-500 dark:focus:ring-primary-500"
      />
    </div>
    <button
      type="button"
      onclick={() => (filtersOpen = true)}
      class="tap relative flex items-center gap-1 rounded-md border border-surface-300 bg-surface-50 px-3 text-sm font-medium dark:border-surface-700 dark:bg-surface-800"
      aria-label="Filters"
    >
      <Filter size={16} />
      Filters
      {#if hasActiveFilters}
        <span class="h-2 w-2 rounded-full bg-primary-500"></span>
      {/if}
    </button>
  </div>

  <!-- Desktop filter bar -->
  <TaskListFilterBar
    bind:searchQuery
    bind:selectedProjectId
    bind:selectedTags
    bind:selectedAgentMode
    bind:showDone
    {allTags}
    {hasActiveFilters}
    {viewMode}
    onclear={clearFilters}
    onviewchange={() => { focusedColIdx = -1; focusedRowIdx = -1 }}
  />

  {#if taskStore.loading}
    <p class="m-auto text-sm opacity-60">Loading tasks...</p>
  {:else if taskStore.error}
    <p class="m-auto text-sm text-error-500">{taskStore.error}</p>
  {:else if viewMode === 'list'}
    <TaskListView
      tasks={allFilteredTasks}
      focusedTaskId={focusedTaskId}
      onselect={(id) => onselect(id)}
      onhover={(rowIdx) => { focusedColIdx = 0; focusedRowIdx = rowIdx }}
    />
  {:else if viewMode === 'timeline'}
    <TaskTimeline
      bind:this={timelineRef}
      tasks={allFilteredTasks}
      focusedTaskId={focusedTaskId}
      onselect={(id) => onselect(id)}
      onfocus={(id) => {
        const idx = allFilteredTasks.findIndex(t => t.id === id)
        if (idx >= 0) { focusedColIdx = 0; focusedRowIdx = idx }
      }}
    />
  {:else}
    <TaskBoardView
      visibleColumns={visibleColumns}
      columnTasks={columnTasks}
      focusedTaskId={focusedTaskId}
      collapsedColumns={collapsedColumns}
      onselect={(id) => onselect(id)}
      onmove={moveTask}
      ontogglecolumn={toggleColumn}
    />
  {/if}
</div>

<!-- Shortcut hint bar for focused task -->
{#if focusedTaskId}
  <div class="shrink-0 border-t border-surface-200 bg-surface-100 px-4 py-1.5 text-xs text-surface-400 dark:border-surface-700 dark:bg-surface-900">
    <span class="flex flex-wrap items-center gap-3">
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">Enter</kbd> / <kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">E</kbd> open</span>
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">S</kbd> status</span>
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">P</kbd> priority</span>
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">⇧C</kbd> project</span>
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">⌘I</kbd> sidebar</span>
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">⌘D</kbd> due date</span>
      <span><kbd class="rounded bg-surface-200 px-1 py-0.5 font-mono dark:bg-surface-700">Esc</kbd> deselect</span>
    </span>
  </div>
{/if}

<!-- Mobile filters sheet -->
<MobileSheet open={filtersOpen} onOpenChange={(o) => (filtersOpen = o)} variant="bottom" title="Filters">
  <div class="flex flex-col gap-4 px-5 pb-5">
    {#if projectStore.list.length > 0}
      <div class="flex flex-col gap-2">
        <span class="text-xs font-semibold uppercase tracking-wider text-surface-500">Project</span>
        <select
          bind:value={selectedProjectId}
          class="rounded-lg border border-surface-300 bg-surface-100 px-3 py-3 text-base dark:border-surface-600 dark:bg-surface-700"
        >
          <option value="">All projects</option>
          {#each projectStore.list as p (p.id)}
            <option value={p.id}>{p.owner}/{p.repo}</option>
          {/each}
        </select>
      </div>
    {/if}

    <div class="flex flex-col gap-2">
      <span class="text-xs font-semibold uppercase tracking-wider text-surface-500">Agent mode</span>
      <div class="flex rounded-lg border border-surface-300 dark:border-surface-700">
        {#each agentModes as mode}
          <button
            type="button"
            class="tap flex-1 px-3 py-2.5 text-sm font-medium transition-colors first:rounded-l-lg last:rounded-r-lg {selectedAgentMode === mode.value
              ? 'bg-primary-500 text-white dark:bg-primary-600'
              : 'bg-surface-50 text-surface-600 active:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:active:bg-surface-700'}"
            onclick={() => (selectedAgentMode = mode.value)}
          >
            {mode.label}
          </button>
        {/each}
      </div>
    </div>

    {#if allTags.length > 0}
      <div class="flex flex-col gap-2">
        <span class="text-xs font-semibold uppercase tracking-wider text-surface-500">Tags</span>
        <div class="flex flex-wrap gap-2">
          {#each allTags as tag}
            <button
              type="button"
              class="tap rounded-full border px-3 py-1.5 text-xs font-medium transition-colors {selectedTags.includes(tag)
                ? 'border-primary-500 bg-primary-500 text-white'
                : 'border-surface-300 bg-surface-100 text-surface-600 active:bg-surface-200 dark:border-surface-600 dark:bg-surface-800 dark:text-surface-300 dark:active:bg-surface-700'}"
              onclick={() => (selectedTags.includes(tag) ? removeTag(tag) : addTag(tag))}
            >
              {tag}
            </button>
          {/each}
        </div>
      </div>
    {/if}

    {#if viewMode === 'board'}
      <!-- No Done column on the board — route to the Logbook instead of a
           toggle that would do nothing. -->
      <button
        type="button"
        class="tap flex items-center gap-3 rounded-lg border border-surface-300 bg-surface-100 px-3 py-3 text-left dark:border-surface-600 dark:bg-surface-700"
        onclick={() => { navStore.reset({ kind: 'logbook' }); filtersOpen = false }}
      >
        <span class="text-sm font-medium">Done → Logbook</span>
      </button>
    {:else}
      <label class="tap flex items-center gap-3 rounded-lg border border-surface-300 bg-surface-100 px-3 py-3 dark:border-surface-600 dark:bg-surface-700">
        <input type="checkbox" bind:checked={showDone} class="h-5 w-5 accent-primary-500" />
        <span class="text-sm font-medium">Show done</span>
      </label>
    {/if}

    <div class="sticky bottom-0 -mx-5 -mb-5 flex gap-2 border-t border-surface-200 bg-surface-50/95 px-5 pt-3 pb-safe backdrop-blur dark:border-surface-800 dark:bg-surface-900/95">
      <button
        type="button"
        onclick={() => { clearFilters(); filtersOpen = false }}
        class="tap flex-1 rounded-lg border border-surface-300 px-4 py-2.5 text-sm font-medium active:bg-surface-200 dark:border-surface-600 dark:active:bg-surface-700"
      >
        Clear
      </button>
      <button
        type="button"
        onclick={() => (filtersOpen = false)}
        class="tap flex-1 rounded-lg bg-primary-500 px-4 py-2.5 text-sm font-medium text-white active:bg-primary-700"
      >
        Done
      </button>
    </div>
  </div>
</MobileSheet>
