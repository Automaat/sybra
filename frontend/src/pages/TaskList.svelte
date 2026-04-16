<script lang="ts">
  import { Search, Filter, ChevronDown, List, Columns, GanttChart } from '@lucide/svelte'
  import type { task } from '../../wailsjs/go/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { BOARD_COLUMNS } from '../lib/statuses.js'
  import { navStore } from '../lib/navigation.svelte.js'
  import TaskCard from '../components/TaskCard.svelte'
  import TaskTimeline from '../components/TaskTimeline.svelte'
  import StatusPicker from '../components/StatusPicker.svelte'
  import PriorityPicker from '../components/PriorityPicker.svelte'
  import AssignProjectDialog from '../components/AssignProjectDialog.svelte'
  import MobileSheet from '../components/shell/MobileSheet.svelte'
  import { viewport } from '../lib/viewport.svelte.js'
  import { fly } from 'svelte/transition'
  import { flip } from 'svelte/animate'
  import { cubicOut } from 'svelte/easing'
  import { PRIORITY_OPTIONS } from '../lib/priorities.js'
  import { viewModeStore } from '../lib/view-mode.svelte.js'

  interface Props {
    onselect: (id: string) => void
    filter?: 'in-progress'
    onnewTask?: () => void
    onfocusedtaskchange?: (taskId: string | null) => void
  }

  const { onselect, filter, onnewTask, onfocusedtaskchange }: Props = $props()

  let dragOverStatus = $state<string | null>(null)
  let addingToColumn = $state<string | null>(null)
  let newTaskTitle = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)
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

  function getColumnTasks(colIndex: number): task.Task[] {
    const col = visibleColumns[colIndex]
    if (!col) return []
    const statuses = col.includes.length > 0 ? col.includes : [col.status]
    return filteredByStatuses(statuses)
  }

  const focusedTaskId = $derived.by((): string | null => {
    if (focusedColIdx < 0 || focusedRowIdx < 0) return null
    const tasks = viewMode === 'list' || viewMode === 'timeline'
      ? allFilteredTasks
      : getColumnTasks(focusedColIdx)
    return tasks[focusedRowIdx]?.id ?? null
  })

  const focusedTask = $derived(focusedTaskId ? taskStore.tasks.get(focusedTaskId) ?? null : null)

  $effect(() => {
    onfocusedtaskchange?.(focusedTaskId)
  })

  function focusFirstTask(): void {
    if (viewMode === 'list' || viewMode === 'timeline') {
      if (allFilteredTasks.length > 0) { focusedColIdx = 0; focusedRowIdx = 0 }
      return
    }
    for (let ci = 0; ci < visibleColumns.length; ci++) {
      if (getColumnTasks(ci).length > 0) {
        focusedColIdx = ci
        focusedRowIdx = 0
        return
      }
    }
  }

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

  function handleBoardKeydown(e: KeyboardEvent): void {
    const target = e.target as HTMLElement
    if (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable) return

    // Don't handle if a picker is open — pickers capture keys themselves
    if (statusPickerOpen || priorityPickerOpen || assignProjectOpen) return

    const key = e.key
    const isCmd = e.metaKey || e.ctrlKey

    // Cmd+D → due date for focused task (navigate to task detail and open due date)
    if (isCmd && key === 'd' && focusedTaskId) {
      e.preventDefault()
      onselect(focusedTaskId)
      requestAnimationFrame(() => window.dispatchEvent(new CustomEvent('open-due-date')))
      return
    }

    if (isCmd) return
    if (e.altKey) return

    if (key === '/' || key === 'F') {
      e.preventDefault()
      searchInputRef?.focus()
      searchInputRef?.select()
      return
    }

    if (viewMode === 'list' || viewMode === 'timeline') {
      if (key === 'j' || key === 'ArrowDown') {
        e.preventDefault()
        if (focusedColIdx < 0) { focusFirstTask(); scrollFocusedIntoView(); return }
        focusedRowIdx = Math.min(focusedRowIdx + 1, allFilteredTasks.length - 1)
        scrollFocusedIntoView()
        return
      }
      if (key === 'k' || key === 'ArrowUp') {
        e.preventDefault()
        if (focusedColIdx < 0) { focusFirstTask(); scrollFocusedIntoView(); return }
        focusedRowIdx = Math.max(focusedRowIdx - 1, 0)
        scrollFocusedIntoView()
        return
      }
      if (viewMode === 'timeline') {
        if (key === '+' || key === '=') {
          e.preventDefault()
          timelineRef?.cycleZoomIn()
          return
        }
        if (key === '-') {
          e.preventDefault()
          timelineRef?.cycleZoomOut()
          return
        }
      }
    } else {
      if (key === 'j' || key === 'ArrowDown') {
        e.preventDefault()
        if (focusedColIdx < 0) { focusFirstTask(); scrollFocusedIntoView(); return }
        const tasks = getColumnTasks(focusedColIdx)
        focusedRowIdx = Math.min(focusedRowIdx + 1, tasks.length - 1)
        scrollFocusedIntoView()
        return
      }

      if (key === 'k' || key === 'ArrowUp') {
        e.preventDefault()
        if (focusedColIdx < 0) { focusFirstTask(); scrollFocusedIntoView(); return }
        focusedRowIdx = Math.max(focusedRowIdx - 1, 0)
        scrollFocusedIntoView()
        return
      }

      if (key === 'h' || key === 'ArrowLeft') {
        e.preventDefault()
        if (focusedColIdx < 0) { focusFirstTask(); scrollFocusedIntoView(); return }
        for (let ci = focusedColIdx - 1; ci >= 0; ci--) {
          const tasks = getColumnTasks(ci)
          if (tasks.length > 0) {
            focusedColIdx = ci
            focusedRowIdx = Math.min(focusedRowIdx, tasks.length - 1)
            scrollFocusedIntoView()
            return
          }
        }
        return
      }

      if (key === 'l' || key === 'ArrowRight') {
        e.preventDefault()
        if (focusedColIdx < 0) { focusFirstTask(); scrollFocusedIntoView(); return }
        for (let ci = focusedColIdx + 1; ci < visibleColumns.length; ci++) {
          const tasks = getColumnTasks(ci)
          if (tasks.length > 0) {
            focusedColIdx = ci
            focusedRowIdx = Math.min(focusedRowIdx, tasks.length - 1)
            scrollFocusedIntoView()
            return
          }
        }
        return
      }
    }

    if (key === 'Enter' || key === 'e') {
      const taskId = focusedTaskId
      if (taskId) {
        e.preventDefault()
        onselect(taskId)
      }
      return
    }

    if (key === 'c' && !e.shiftKey) {
      e.preventDefault()
      onnewTask?.()
      return
    }

    if (key === 'C' && e.shiftKey) {
      e.preventDefault()
      if (focusedTaskId) assignProjectOpen = true
      return
    }

    if (key === 's' && focusedTaskId) {
      e.preventDefault()
      statusPickerOpen = true
      return
    }

    if (key === 'p' && focusedTaskId) {
      e.preventDefault()
      priorityPickerOpen = true
      return
    }

    if (key === 'Escape') {
      focusedColIdx = -1
      focusedRowIdx = -1
      return
    }
  }

  $effect(() => {
    window.addEventListener('keydown', handleBoardKeydown)
    return () => window.removeEventListener('keydown', handleBoardKeydown)
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

  let searchInputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    function onFocusSearch() {
      searchInputRef?.focus()
      searchInputRef?.select()
    }
    window.addEventListener('focus-search', onFocusSearch)
    return () => window.removeEventListener('focus-search', onFocusSearch)
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
    [...new Set(taskStore.list.flatMap((t: task.Task) => t.tags ?? []))].sort()
  )

  // Derived: filter function
  function filteredByStatuses(statuses: string[]): task.Task[] {
    const query = searchQuery.toLowerCase().trim()
    return taskStore.list.filter((t: task.Task) => {
      if (!statuses.includes(t.status)) return false
      if (query && !t.title.toLowerCase().includes(query)
          && !(t.body ?? '').toLowerCase().includes(query)
          && !(t.issue ?? '').toLowerCase().includes(query)) return false
      if (selectedProjectId && t.projectId !== selectedProjectId) return false
      if (selectedTags.length > 0 && !selectedTags.every(tag => t.tags?.includes(tag))) return false
      if (selectedAgentMode && t.agentMode !== selectedAgentMode) return false
      return true
    })
  }

  // All filtered tasks for list view (sorted by status order)
  const statusOrder: Record<string, number> = {
    'new': 0, 'todo': 1, 'planning': 2, 'plan-review': 3,
    'in-progress': 4, 'in-review': 5, 'testing': 6, 'test-plan-review': 7,
    'human-required': 8, 'done': 9,
  }
  const priorityOrder: Record<string, number> = {
    'urgent': 0, 'high': 1, 'medium': 2, 'low': 3, '': 4,
  }

  const allFilteredTasks = $derived.by(() => {
    const query = searchQuery.toLowerCase().trim()
    return taskStore.list.filter((t: task.Task) => {
      if (!showDone && (t.status === 'done' || t.status === 'cancelled')) return false
      if (query && !t.title.toLowerCase().includes(query)
          && !(t.body ?? '').toLowerCase().includes(query)
          && !(t.issue ?? '').toLowerCase().includes(query)) return false
      if (selectedProjectId && t.projectId !== selectedProjectId) return false
      if (selectedTags.length > 0 && !selectedTags.every(tag => t.tags?.includes(tag))) return false
      if (selectedAgentMode && t.agentMode !== selectedAgentMode) return false
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
    searchQuery || selectedProjectId || selectedTags.length > 0 || selectedAgentMode
  )

  function clearFilters() {
    searchQuery = ''
    selectedProjectId = ''
    selectedTags = []
    selectedAgentMode = ''
  }

  // Project dropdown
  let projectDropdownOpen = $state(false)
  let projectDropdownRef = $state<HTMLDivElement | null>(null)

  function handleWindowClick(e: MouseEvent) {
    if (projectDropdownOpen && projectDropdownRef && !projectDropdownRef.contains(e.target as Node)) {
      projectDropdownOpen = false
    }
  }

  const selectedProjectLabel = $derived(
    selectedProjectId
      ? projectStore.list.find(p => p.id === selectedProjectId)
          ? `${projectStore.list.find(p => p.id === selectedProjectId)!.owner}/${projectStore.list.find(p => p.id === selectedProjectId)!.repo}`
          : selectedProjectId
      : 'All projects'
  )

  // Tag input with autosuggest
  let tagInput = $state('')
  let tagInputFocused = $state(false)
  let tagInputRef = $state<HTMLInputElement | null>(null)

  const tagSuggestions = $derived(
    tagInput.trim()
      ? allTags.filter(t => t.toLowerCase().includes(tagInput.toLowerCase()) && !selectedTags.includes(t))
      : []
  )

  function addTag(tag: string) {
    if (!selectedTags.includes(tag)) {
      selectedTags = [...selectedTags, tag]
    }
    tagInput = ''
  }

  function removeTag(tag: string) {
    selectedTags = selectedTags.filter(t => t !== tag)
  }

  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && tagSuggestions.length > 0) {
      e.preventDefault()
      addTag(tagSuggestions[0])
    } else if (e.key === 'Backspace' && !tagInput && selectedTags.length > 0) {
      selectedTags = selectedTags.slice(0, -1)
    } else if (e.key === 'Escape') {
      tagInputRef?.blur()
    }
  }

  const agentModes = [
    { value: '', label: 'All' },
    { value: 'headless', label: 'Headless' },
    { value: 'interactive', label: 'Interactive' },
  ]

  async function handleDrop(e: DragEvent, targetStatus: string) {
    e.preventDefault()
    dragOverStatus = null
    const taskId = e.dataTransfer?.getData('text/plain')
    if (!taskId) return
    const existing = taskStore.tasks.get(taskId)
    if (!existing || existing.status === targetStatus) return
    try {
      await taskStore.update(taskId, { status: targetStatus })
    } catch (err) {
      notificationStore.pushLocal('error', 'Move failed', String(err))
    }
  }

  function openInlineAdd(status: string) {
    addingToColumn = status
    newTaskTitle = ''
    requestAnimationFrame(() => inputRef?.focus())
  }

  function dismissInlineAdd() {
    addingToColumn = null
    newTaskTitle = ''
  }

  async function submitInlineAdd(status: string) {
    const title = newTaskTitle.trim()
    if (!title) return
    newTaskTitle = ''
    const created = await taskStore.create(title, '', 'headless')
    if (status !== 'new') {
      await taskStore.update(created.id, { status })
    }
    requestAnimationFrame(() => inputRef?.focus())
  }

  function handleInputKeydown(e: KeyboardEvent, status: string) {
    if (e.key === 'Enter') {
      e.preventDefault()
      submitInlineAdd(status)
    } else if (e.key === 'Escape') {
      dismissInlineAdd()
    }
  }

  function priorityIcon(p: string | undefined): string {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? ''))?.icon ?? '–'
  }

  function priorityLabel(p: string | undefined): string {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? ''))?.label ?? 'None'
  }

  function priorityClasses(p: string | undefined): string {
    return PRIORITY_OPTIONS.find(o => o.value === (p ?? ''))?.classes ?? 'text-surface-400'
  }
</script>

<svelte:window onclick={handleWindowClick} />

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
  <div class="hidden flex-wrap items-center gap-3 border-b border-surface-200 px-6 py-3 dark:border-surface-800 md:flex">
    <!-- Search -->
    <div class="relative">
      <Search size={16} class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-surface-400" />
      <input
        bind:this={searchInputRef}
        type="text"
        bind:value={searchQuery}
        placeholder="Search tasks..."
        class="h-8 w-56 rounded-md border border-surface-300 bg-surface-50 pl-8 pr-2 text-sm outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-700 dark:bg-surface-800 dark:focus:border-primary-500 dark:focus:ring-primary-500"
      />
    </div>

    <!-- Project filter -->
    {#if projectStore.list.length > 0}
      <div class="relative" bind:this={projectDropdownRef}>
        <button
          type="button"
          class="flex h-8 items-center gap-2 rounded-md border border-surface-300 bg-surface-50 px-2.5 text-sm dark:border-surface-700 dark:bg-surface-800"
          onclick={() => (projectDropdownOpen = !projectDropdownOpen)}
        >
          <span class={selectedProjectId ? '' : 'text-surface-400'}>{selectedProjectLabel}</span>
          <ChevronDown size={14} class="text-surface-400" />
        </button>
        {#if projectDropdownOpen}
          <div class="absolute top-full z-10 mt-1 min-w-full rounded-md border border-surface-300 bg-surface-50 py-1 shadow-lg dark:border-surface-700 dark:bg-surface-800">
            <button
              type="button"
              class="w-full whitespace-nowrap px-3 py-1.5 text-left text-sm hover:bg-surface-200 dark:hover:bg-surface-700 {selectedProjectId === '' ? 'font-medium text-primary-500' : ''}"
              onmousedown={() => { selectedProjectId = ''; projectDropdownOpen = false }}
            >
              All projects
            </button>
            {#each projectStore.list as p}
              <button
                type="button"
                class="w-full whitespace-nowrap px-3 py-1.5 text-left text-sm hover:bg-surface-200 dark:hover:bg-surface-700 {selectedProjectId === p.id ? 'font-medium text-primary-500' : ''}"
                onmousedown={() => { selectedProjectId = p.id; projectDropdownOpen = false }}
              >
                {p.owner}/{p.repo}
              </button>
            {/each}
          </div>
        {/if}
      </div>
    {/if}

    <!-- Agent mode pills -->
    <div class="flex h-8 rounded-md border border-surface-300 dark:border-surface-700">
      {#each agentModes as mode}
        <button
          type="button"
          class="px-2.5 text-xs font-medium transition-colors first:rounded-l-md last:rounded-r-md {selectedAgentMode === mode.value
            ? 'bg-primary-500 text-white dark:bg-primary-600'
            : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
          onclick={() => (selectedAgentMode = mode.value)}
        >
          {mode.label}
        </button>
      {/each}
    </div>

    <!-- Tag filter -->
    <div class="relative">
      <div class="flex h-8 flex-wrap items-center gap-1 rounded-md border border-surface-300 bg-surface-50 px-2 dark:border-surface-700 dark:bg-surface-800">
        {#each selectedTags as tag}
          <span class="inline-flex items-center gap-1 rounded bg-primary-500 px-1.5 py-0.5 text-xs font-medium text-white dark:bg-primary-600">
            {tag}
            <button type="button" class="hover:text-primary-200" onclick={() => removeTag(tag)}>&times;</button>
          </span>
        {/each}
        <input
          bind:this={tagInputRef}
          bind:value={tagInput}
          type="text"
          placeholder={selectedTags.length ? '' : 'Filter by tag...'}
          class="min-w-[80px] flex-1 bg-transparent py-0.5 text-sm outline-none"
          onfocus={() => (tagInputFocused = true)}
          onblur={() => setTimeout(() => (tagInputFocused = false), 150)}
          onkeydown={handleTagKeydown}
        />
      </div>
      {#if tagInputFocused && tagSuggestions.length > 0}
        <div class="absolute top-full z-10 mt-1 w-full rounded-md border border-surface-300 bg-surface-50 py-1 shadow-lg dark:border-surface-700 dark:bg-surface-800">
          {#each tagSuggestions as suggestion}
            <button
              type="button"
              class="w-full px-3 py-1.5 text-left text-sm hover:bg-surface-200 dark:hover:bg-surface-700"
              onmousedown={() => addTag(suggestion)}
            >
              {suggestion}
            </button>
          {/each}
        </div>
      {/if}
    </div>

    <!-- Right side: clear + show done + view toggle -->
    <div class="ml-auto flex items-center gap-3">
      {#if hasActiveFilters}
        <button
          type="button"
          class="text-xs text-surface-500 underline hover:text-surface-700 dark:hover:text-surface-300"
          onclick={clearFilters}
        >
          Clear filters
        </button>
      {/if}
      <button
        type="button"
        class="text-xs text-surface-500 underline hover:text-surface-700 dark:hover:text-surface-300"
        onclick={() => navStore.reset({ kind: 'logbook' })}
      >
        Logbook →
      </button>
      <label class="flex items-center gap-1.5 text-xs text-surface-500">
        <input type="checkbox" bind:checked={showDone} class="accent-primary-500" />
        Show done
      </label>
      <div class="flex rounded-md border border-surface-300 dark:border-surface-700" title="Switch view (⌘B)">
        <button
          type="button"
          class="flex items-center gap-1 px-2 py-1 text-xs font-medium transition-colors first:rounded-l-md last:rounded-r-md {viewMode === 'list' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
          onclick={() => { viewModeStore.set('list'); focusedColIdx = -1; focusedRowIdx = -1 }}
        >
          <List size={14} />
          List
        </button>
        <button
          type="button"
          class="flex items-center gap-1 border-x border-surface-300 px-2 py-1 text-xs font-medium transition-colors dark:border-surface-700 {viewMode === 'board' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
          onclick={() => { viewModeStore.set('board'); focusedColIdx = -1; focusedRowIdx = -1 }}
        >
          <Columns size={14} />
          Board
        </button>
        <button
          type="button"
          class="flex items-center gap-1 px-2 py-1 text-xs font-medium transition-colors first:rounded-l-md last:rounded-r-md {viewMode === 'timeline' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
          onclick={() => { viewModeStore.set('timeline'); focusedColIdx = -1; focusedRowIdx = -1 }}
        >
          <GanttChart size={14} />
          Timeline
        </button>
      </div>
    </div>
  </div>

  {#if taskStore.loading}
    <p class="m-auto text-sm opacity-60">Loading tasks...</p>
  {:else if taskStore.error}
    <p class="m-auto text-sm text-error-500">{taskStore.error}</p>
  {:else if viewMode === 'list'}
    <!-- List view -->
    <div class="min-h-0 flex-1 overflow-y-auto">
      <table class="w-full text-sm">
        <thead class="sticky top-0 z-10 border-b border-surface-200 bg-surface-100 dark:border-surface-700 dark:bg-surface-900">
          <tr>
            <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider w-8">P</th>
            <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider">Title</th>
            <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider">Status</th>
            <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider hidden md:table-cell">Project</th>
            <th class="px-4 py-2 text-left font-semibold text-surface-500 text-xs uppercase tracking-wider hidden lg:table-cell">Updated</th>
          </tr>
        </thead>
        <tbody>
          {#each allFilteredTasks as t, rowIdx (t.id)}
            {@const isFocused = focusedTaskId === t.id}
            <tr
              data-focused-task={isFocused ? '' : undefined}
              class="cursor-pointer border-b border-surface-100 transition-colors dark:border-surface-800 {isFocused ? 'bg-primary-50 dark:bg-primary-900/20' : 'hover:bg-surface-100 dark:hover:bg-surface-800'}"
              onclick={() => onselect(t.id)}
              onmouseenter={() => { focusedColIdx = 0; focusedRowIdx = rowIdx }}
            >
              <td class="px-4 py-2">
                <span class="font-mono text-sm {priorityClasses(t.priority)}" title="Priority: {priorityLabel(t.priority)}">{priorityIcon(t.priority)}</span>
              </td>
              <td class="px-4 py-2 font-medium">{t.title}</td>
              <td class="px-4 py-2">
                <span class="rounded-full px-2 py-0.5 text-xs font-semibold bg-surface-200 dark:bg-surface-700">{t.status}</span>
              </td>
              <td class="hidden px-4 py-2 text-surface-500 md:table-cell">{t.projectId || '—'}</td>
              <td class="hidden px-4 py-2 text-surface-400 text-xs lg:table-cell">
                {t.updatedAt ? new Date(t.updatedAt).toLocaleDateString() : '—'}
              </td>
            </tr>
          {/each}
          {#if allFilteredTasks.length === 0}
            <tr>
              <td colspan="5" class="px-4 py-8 text-center text-surface-400">No tasks match your filters</td>
            </tr>
          {/if}
        </tbody>
      </table>
    </div>
  {:else if viewMode === 'timeline'}
    <!-- Timeline / Gantt view -->
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
    <!-- Board columns -->
    <div class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3 md:flex-row md:gap-4 md:overflow-x-auto md:overflow-y-hidden md:p-6">
      {#each visibleColumns as col}
        {@const statuses = col.includes.length > 0 ? col.includes : [col.status]}
        {@const tasks = filteredByStatuses(statuses)}
        {@const isCollapsed = !viewport.isDesktop && collapsedColumns.has(col.status)}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          data-col-status={col.status}
          class="flex w-full shrink-0 flex-col rounded-lg border-t-4 bg-surface-100 transition-shadow dark:bg-surface-900 md:min-w-[260px] md:flex-1 md:shrink {col.border} {dragOverStatus === col.status ? 'ring-2 ring-primary-400 dark:ring-primary-500' : ''}"
          ondragover={(e) => { e.preventDefault(); dragOverStatus = col.status }}
          ondragleave={() => { dragOverStatus = null }}
          ondrop={(e) => handleDrop(e, col.status)}
        >
          <button
            type="button"
            onclick={() => toggleColumn(col.status)}
            class="tap flex w-full items-center justify-between gap-2 px-3 py-2 text-left active:bg-surface-200 dark:active:bg-surface-800 md:cursor-default md:active:bg-transparent dark:md:active:bg-transparent"
          >
            <span class="flex items-center gap-2">
              <ChevronDown size={16} class="transition-transform md:hidden {isCollapsed ? '-rotate-90' : ''}" aria-hidden="true" />
              <h2 class="text-sm font-semibold">{col.label}</h2>
            </span>
            <span class="rounded-full bg-surface-200 px-2 py-0.5 text-xs font-medium dark:bg-surface-700">
              {tasks.length}
            </span>
          </button>
          {#if !isCollapsed}
            <div class="flex flex-col gap-2 px-2 pb-2 md:flex-1 md:overflow-y-auto">
              {#each tasks as t (t.id)}
                <div
                  in:fly={{ y: -12, duration: 150, easing: cubicOut }}
                  out:fly={{ y: 12, duration: 200, easing: cubicOut }}
                  animate:flip={{ duration: 200, easing: cubicOut }}
                >
                  <TaskCard
                    task={t}
                    onclick={() => onselect(t.id)}
                    focused={focusedTaskId === t.id}
                    onstatuschange={(s) => moveTask(t.id, s)}
                  />
                </div>
              {/each}
            </div>
            <div class="px-2 pb-2">
              {#if addingToColumn === col.status}
                <input
                  bind:this={inputRef}
                  bind:value={newTaskTitle}
                  type="text"
                  placeholder="Task title"
                  class="w-full rounded-md border border-surface-300 bg-surface-50 px-2 py-2.5 text-base outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-600 dark:bg-surface-800 md:py-1.5 md:text-sm"
                  onkeydown={(e) => handleInputKeydown(e, col.status)}
                  onblur={() => dismissInlineAdd()}
                />
              {:else}
                <button
                  type="button"
                  class="tap flex w-full items-center gap-1 rounded-md px-2 py-2.5 text-sm opacity-60 transition-opacity active:bg-surface-200 active:opacity-100 dark:active:bg-surface-800 md:py-1.5 md:hover:bg-surface-200 md:hover:opacity-100 dark:md:hover:bg-surface-800"
                  onclick={() => openInlineAdd(col.status)}
                  title="Add task (C)"
                >
                  <span class="text-base leading-none">+</span> Add task
                </button>
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    </div>
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

    <label class="tap flex items-center gap-3 rounded-lg border border-surface-300 bg-surface-100 px-3 py-3 dark:border-surface-600 dark:bg-surface-700">
      <input type="checkbox" bind:checked={showDone} class="h-5 w-5 accent-primary-500" />
      <span class="text-sm font-medium">Show done</span>
    </label>

    <div class="sticky bottom-0 -mx-5 -mb-5 flex gap-2 border-t border-surface-200 bg-surface-50/95 px-5 pt-3 pb-safe backdrop-blur dark:border-surface-800 dark:bg-surface-950/95">
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
