<script lang="ts">
  import type { task } from '../../wailsjs/go/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { BOARD_COLUMNS } from '../lib/statuses.js'
  import TaskCard from '../components/TaskCard.svelte'
  import MobileSheet from '../components/shell/MobileSheet.svelte'
  import { viewport } from '../lib/viewport.svelte.js'
  import { fly } from 'svelte/transition'
  import { flip } from 'svelte/animate'
  import { cubicOut } from 'svelte/easing'

  interface Props {
    onselect: (id: string) => void
  }

  const { onselect }: Props = $props()

  let dragOverStatus = $state<string | null>(null)
  let addingToColumn = $state<string | null>(null)
  let newTaskTitle = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)
  let filtersOpen = $state(false)
  let collapsedColumns = $state<Set<string>>(new Set(['testing', 'done']))

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

  function getColumnTasks(colIndex: number): task.Task[] {
    const col = visibleColumns[colIndex]
    if (!col) return []
    const statuses = col.includes.length > 0 ? col.includes : [col.status]
    return filteredByStatuses(statuses)
  }

  const focusedTaskId = $derived.by((): string | null => {
    if (focusedColIdx < 0 || focusedRowIdx < 0) return null
    const tasks = getColumnTasks(focusedColIdx)
    return tasks[focusedRowIdx]?.id ?? null
  })

  function focusFirstTask(): void {
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

  function handleBoardKeydown(e: KeyboardEvent): void {
    const target = e.target as HTMLElement
    if (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable) return
    if (e.metaKey || e.ctrlKey || e.altKey) return

    const key = e.key

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

    if (key === 'Enter') {
      const taskId = focusedTaskId
      if (taskId) {
        e.preventDefault()
        onselect(taskId)
      }
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

  let searchInputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    function onFocusSearch() {
      searchInputRef?.focus()
      searchInputRef?.select()
    }
    window.addEventListener('focus-search', onFocusSearch)
    return () => window.removeEventListener('focus-search', onFocusSearch)
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
</script>

<svelte:window onclick={handleWindowClick} />

<div class="flex h-full min-h-0 flex-col">
  <!-- Mobile filter trigger -->
  <div class="flex shrink-0 items-center gap-2 border-b border-surface-200 px-3 py-2 dark:border-surface-800 md:hidden">
    <div class="relative flex-1">
      <svg class="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
      </svg>
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
      <svg class="h-4 w-4" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" d="M3 4a1 1 0 011-1h16a1 1 0 011 1v2.586a1 1 0 01-.293.707l-6.414 6.414a1 1 0 00-.293.707V17l-4 4v-6.586a1 1 0 00-.293-.707L3.293 7.293A1 1 0 013 6.586V4z" />
      </svg>
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
      <svg class="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
      </svg>
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
          <svg class="h-3.5 w-3.5 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
          </svg>
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

    <!-- Right side: clear + show done -->
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
      <label class="flex items-center gap-1.5 text-xs text-surface-500">
        <input type="checkbox" bind:checked={showDone} class="accent-primary-500" />
        Show done
      </label>
    </div>
  </div>

  <!-- Board columns -->
  <div class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3 md:flex-row md:gap-4 md:overflow-x-auto md:overflow-y-hidden md:p-6">
    {#if taskStore.loading}
      <p class="m-auto text-sm opacity-60">Loading tasks...</p>
    {:else if taskStore.error}
      <p class="m-auto text-sm text-error-500">{taskStore.error}</p>
    {:else}
      {#each visibleColumns as col}
        {@const statuses = col.includes.length > 0 ? col.includes : [col.status]}
        {@const tasks = filteredByStatuses(statuses)}
        {@const isCollapsed = !viewport.isDesktop && collapsedColumns.has(col.status)}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
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
              <svg
                class="h-4 w-4 transition-transform md:hidden {isCollapsed ? '-rotate-90' : ''}"
                fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24"
                aria-hidden="true"
              >
                <path stroke-linecap="round" stroke-linejoin="round" d="M19 9l-7 7-7-7" />
              </svg>
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
                >
                  <span class="text-base leading-none">+</span> Add task
                </button>
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    {/if}
  </div>
</div>

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
