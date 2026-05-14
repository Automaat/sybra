<script lang="ts">
  import { List, Columns, GanttChart } from '@lucide/svelte'
  import TaskFilterPanel from './TaskFilterPanel.svelte'
  import { navStore } from '../lib/navigation.svelte.js'
  import { viewModeStore, type ViewMode } from '../lib/view-mode.svelte.js'

  interface Props {
    searchQuery: string
    selectedProjectId: string
    selectedTags: string[]
    selectedAgentMode: string
    allTags: string[]
    showDone: boolean
    hasActiveFilters: boolean
    viewMode: ViewMode
    onclear: () => void
    onviewchange?: () => void
  }

  let {
    searchQuery = $bindable(),
    selectedProjectId = $bindable(),
    selectedTags = $bindable(),
    selectedAgentMode = $bindable(),
    allTags,
    showDone = $bindable(),
    hasActiveFilters,
    viewMode,
    onclear,
    onviewchange,
  }: Props = $props()

  const agentModes = [
    { value: '', label: 'All' },
    { value: 'headless', label: 'Headless' },
    { value: 'interactive', label: 'Interactive' },
  ]

  function pickViewMode(m: ViewMode) {
    viewModeStore.set(m)
    onviewchange?.()
  }
</script>

<div class="hidden flex-wrap items-center gap-3 border-b border-surface-200 px-6 py-3 dark:border-surface-800 md:flex">
  <TaskFilterPanel
    query={searchQuery}
    onqueryChange={(q) => (searchQuery = q)}
    focusEvent="focus-search"
    showProject
    selectedProjectId={selectedProjectId}
    onprojectChange={(id) => (selectedProjectId = id)}
    showTags
    availableTags={allTags}
    selectedTags={selectedTags}
    ontagsChange={(tags) => (selectedTags = tags)}
  />

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

  <div class="ml-auto flex items-center gap-3">
    {#if hasActiveFilters}
      <button
        type="button"
        class="text-xs text-surface-500 underline hover:text-surface-700 dark:hover:text-surface-300"
        onclick={onclear}
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
        onclick={() => pickViewMode('list')}
      >
        <List size={14} />
        List
      </button>
      <button
        type="button"
        class="flex items-center gap-1 border-x border-surface-300 px-2 py-1 text-xs font-medium transition-colors dark:border-surface-700 {viewMode === 'board' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
        onclick={() => pickViewMode('board')}
      >
        <Columns size={14} />
        Board
      </button>
      <button
        type="button"
        class="flex items-center gap-1 px-2 py-1 text-xs font-medium transition-colors first:rounded-l-md last:rounded-r-md {viewMode === 'timeline' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
        onclick={() => pickViewMode('timeline')}
      >
        <GanttChart size={14} />
        Timeline
      </button>
    </div>
  </div>
</div>
