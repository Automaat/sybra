<script lang="ts">
  import type { task } from '../../wailsjs/go/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { notificationStore } from '../stores/notifications.svelte.js'
  import { BOARD_COLUMNS } from '../lib/statuses.js'
  import TaskCard from '../components/TaskCard.svelte'

  interface Props {
    onselect: (id: string) => void
  }

  const { onselect }: Props = $props()

  let dragOverStatus = $state<string | null>(null)
  let addingToColumn = $state<string | null>(null)
  let newTaskTitle = $state('')
  let inputRef = $state<HTMLInputElement | null>(null)

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

<div class="flex h-full flex-col">
  <!-- Filter bar -->
  <div class="flex flex-wrap items-center gap-3 border-b border-surface-200 px-6 py-3 dark:border-surface-800">
    <!-- Search -->
    <div class="relative">
      <svg class="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
      </svg>
      <input
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
  <div class="flex min-h-0 flex-1 gap-4 overflow-x-auto p-6">
    {#if taskStore.loading}
      <p class="m-auto text-sm opacity-60">Loading tasks...</p>
    {:else if taskStore.error}
      <p class="m-auto text-sm text-error-500">{taskStore.error}</p>
    {:else}
      {#each visibleColumns as col}
        {@const statuses = col.includes.length > 0 ? col.includes : [col.status]}
        {@const tasks = filteredByStatuses(statuses)}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <div
          class="flex min-w-[250px] flex-1 flex-col rounded-lg border-t-4 bg-surface-100 transition-shadow dark:bg-surface-900 {col.border} {dragOverStatus === col.status ? 'ring-2 ring-primary-400 dark:ring-primary-500' : ''}"
          ondragover={(e) => { e.preventDefault(); dragOverStatus = col.status }}
          ondragleave={() => { dragOverStatus = null }}
          ondrop={(e) => handleDrop(e, col.status)}
        >
          <div class="flex items-center justify-between px-3 py-2">
            <h2 class="text-sm font-semibold">{col.label}</h2>
            <span class="rounded-full bg-surface-200 px-2 py-0.5 text-xs font-medium dark:bg-surface-700">
              {tasks.length}
            </span>
          </div>
          <div class="flex flex-1 flex-col gap-2 overflow-y-auto px-2 pb-2">
            {#each tasks as t (t.id)}
              <TaskCard task={t} onclick={() => onselect(t.id)} />
            {/each}
          </div>
          <div class="px-2 pb-2">
            {#if addingToColumn === col.status}
              <input
                bind:this={inputRef}
                bind:value={newTaskTitle}
                type="text"
                placeholder="Task title"
                class="w-full rounded-md border border-surface-300 bg-surface-50 px-2 py-1.5 text-sm outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-600 dark:bg-surface-800"
                onkeydown={(e) => handleInputKeydown(e, col.status)}
                onblur={() => dismissInlineAdd()}
              />
            {:else}
              <button
                type="button"
                class="flex w-full items-center gap-1 rounded-md px-2 py-1.5 text-sm opacity-50 transition-opacity hover:bg-surface-200 hover:opacity-100 dark:hover:bg-surface-800"
                onclick={() => openInlineAdd(col.status)}
              >
                <span class="text-base leading-none">+</span> Add task
              </button>
            {/if}
          </div>
        </div>
      {/each}
    {/if}
  </div>
</div>
