<script lang="ts">
  import { List, Columns, GanttChart } from '@lucide/svelte'
  import TaskFilterPanel from './TaskFilterPanel.svelte'
  import { navStore } from '../lib/navigation.svelte.js'
  import { viewModeStore, type ViewMode } from '../lib/view-mode.svelte.js'
  import { focusModeStore } from '../lib/focus-mode.svelte.js'

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
    {#if viewMode === 'board'}
      <!-- The board has no Done column (terminal tasks live in the Logbook), so
           a "Show done" toggle here does nothing visible. Route there instead. -->
      <button
        type="button"
        class="text-xs text-surface-500 underline hover:text-surface-700 dark:hover:text-surface-300"
        onclick={() => navStore.reset({ kind: 'logbook' })}
      >
        Done → Logbook
      </button>
    {:else}
      <label class="flex items-center gap-1.5 text-xs text-surface-500">
        <input type="checkbox" bind:checked={showDone} class="accent-primary-500" />
        Show done
      </label>
    {/if}
    <!-- Primary views: List / Board. Timeline is demoted to an advanced option. -->
    <div class="flex rounded-md border border-surface-300 dark:border-surface-700" title="Switch view (⌘B)">
      <button
        type="button"
        aria-label="List view"
        class="flex items-center gap-1 rounded-l-md px-2 py-1 text-xs font-medium transition-colors {viewMode === 'list' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
        onclick={() => pickViewMode('list')}
      >
        <List size={14} />
        List
      </button>
      <button
        type="button"
        aria-label="Board view"
        class="flex items-center gap-1 rounded-r-md border-l border-surface-300 px-2 py-1 text-xs font-medium transition-colors dark:border-surface-700 {viewMode === 'board' ? 'bg-primary-500 text-white dark:bg-primary-600' : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
        onclick={() => pickViewMode('board')}
      >
        <Columns size={14} />
        Board
      </button>
    </div>
    {#if !focusModeStore.enabled}
      <!-- Advanced: the Timeline (Gantt) is de-emphasized and hidden while focus
           mode is on (focus mode leads with the list view on enable). -->
      <button
        type="button"
        aria-label="Timeline view (advanced)"
        title="Timeline — Gantt of tasks and agent runs over time, which the list and board can't show"
        class="flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium transition-colors {viewMode === 'timeline'
          ? 'bg-primary-500 text-white dark:bg-primary-600'
          : 'text-surface-400 hover:bg-surface-200 hover:text-surface-600 dark:text-surface-500 dark:hover:bg-surface-700 dark:hover:text-surface-300'}"
        onclick={() => pickViewMode('timeline')}
      >
        <GanttChart size={14} />
        Timeline
      </button>
    {/if}
  </div>
</div>
