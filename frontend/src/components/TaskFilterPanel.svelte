<script lang="ts">
  import { Search, ChevronDown } from '@lucide/svelte'
  import { projectStore } from '../stores/projects.svelte.js'

  interface Props {
    query: string
    onqueryChange: (q: string) => void
    /** When set, the panel listens for this window CustomEvent and focuses the search input. */
    focusEvent?: string

    showProject?: boolean
    selectedProjectId?: string
    onprojectChange?: (id: string) => void

    showTags?: boolean
    availableTags?: string[]
    selectedTags?: string[]
    ontagsChange?: (tags: string[]) => void

    showDateRange?: boolean
    dateFrom?: string
    dateTo?: string
    ondateChange?: (from: string, to: string) => void

    /** Status pill row. Empty/undefined = hidden. */
    statusPills?: { val: string; label: string }[]
    selectedStatus?: string
    onstatusChange?: (s: string) => void

    /** When set, renders a "Clear" link/button. */
    onclear?: () => void
    hasActive?: boolean

    variant?: 'horizontal' | 'vertical'
  }

  const {
    query,
    onqueryChange,
    focusEvent,
    showProject = false,
    selectedProjectId = '',
    onprojectChange,
    showTags = false,
    availableTags = [],
    selectedTags = [],
    ontagsChange,
    showDateRange = false,
    dateFrom = '',
    dateTo = '',
    ondateChange,
    statusPills,
    selectedStatus = '',
    onstatusChange,
    onclear,
    hasActive = false,
    variant = 'horizontal',
  }: Props = $props()

  let projectDropdownOpen = $state(false)
  let projectDropdownRef = $state<HTMLDivElement | null>(null)
  let searchInputRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    if (!focusEvent) return
    function onFocus() {
      searchInputRef?.focus()
      searchInputRef?.select()
    }
    window.addEventListener(focusEvent, onFocus)
    return () => window.removeEventListener(focusEvent, onFocus)
  })

  let tagInput = $state('')
  let tagInputFocused = $state(false)

  const tagSuggestions = $derived(
    tagInput.trim()
      ? availableTags.filter(
          (t) => t.toLowerCase().includes(tagInput.toLowerCase()) && !selectedTags.includes(t),
        )
      : [],
  )

  const selectedProjectLabel = $derived(
    selectedProjectId
      ? (() => {
          const p = projectStore.list.find((x) => x.id === selectedProjectId)
          return p ? `${p.owner}/${p.repo}` : selectedProjectId
        })()
      : 'All projects',
  )

  function addTag(tag: string) {
    if (!ontagsChange) return
    if (!selectedTags.includes(tag)) ontagsChange([...selectedTags, tag])
    tagInput = ''
  }

  function removeTag(tag: string) {
    if (!ontagsChange) return
    ontagsChange(selectedTags.filter((t) => t !== tag))
  }

  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && tagSuggestions.length > 0) {
      e.preventDefault()
      addTag(tagSuggestions[0])
    } else if (e.key === 'Backspace' && !tagInput && selectedTags.length > 0) {
      ontagsChange?.(selectedTags.slice(0, -1))
    }
  }

  function handleWindowClick(e: MouseEvent) {
    if (
      projectDropdownOpen &&
      projectDropdownRef &&
      !projectDropdownRef.contains(e.target as Node)
    ) {
      projectDropdownOpen = false
    }
  }
</script>

<svelte:window onclick={handleWindowClick} />

<div
  class="flex gap-3 {variant === 'horizontal' ? 'flex-wrap items-center' : 'flex-col'}"
  data-testid="task-filter-panel"
>
  <!-- Search -->
  <div class="relative {variant === 'horizontal' ? '' : 'w-full'}">
    <Search size={16} class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-surface-400" />
    <input
      bind:this={searchInputRef}
      type="text"
      value={query}
      oninput={(e) => onqueryChange((e.currentTarget as HTMLInputElement).value)}
      placeholder="Search tasks..."
      class="h-8 {variant === 'horizontal' ? 'w-56' : 'w-full'} rounded-md border border-surface-300 bg-surface-50 pl-8 pr-2 text-sm outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-700 dark:bg-surface-800 dark:focus:border-primary-500 dark:focus:ring-primary-500"
      data-testid="filter-search"
    />
  </div>

  <!-- Status pills -->
  {#if statusPills && statusPills.length > 0}
    <div class="flex h-8 rounded-md border border-surface-300 dark:border-surface-700">
      {#each statusPills as pill}
        <button
          type="button"
          class="px-2.5 text-xs font-medium transition-colors first:rounded-l-md last:rounded-r-md {selectedStatus === pill.val
            ? 'bg-primary-500 text-white dark:bg-primary-600'
            : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
          onclick={() => onstatusChange?.(pill.val)}
          data-testid="status-pill-{pill.val}"
        >
          {pill.label}
        </button>
      {/each}
    </div>
  {/if}

  <!-- Project filter -->
  {#if showProject && projectStore.list.length > 0}
    <div class="relative" bind:this={projectDropdownRef}>
      <button
        type="button"
        class="flex h-8 items-center gap-2 rounded-md border border-surface-300 bg-surface-50 px-2.5 text-sm dark:border-surface-700 dark:bg-surface-800"
        onclick={() => (projectDropdownOpen = !projectDropdownOpen)}
        data-testid="project-filter-button"
      >
        <span class={selectedProjectId ? '' : 'text-surface-400'}>{selectedProjectLabel}</span>
        <ChevronDown size={14} class="text-surface-400" />
      </button>
      {#if projectDropdownOpen}
        <div class="absolute top-full z-10 mt-1 min-w-full rounded-md border border-surface-300 bg-surface-50 py-1 shadow-lg dark:border-surface-700 dark:bg-surface-800">
          <button
            type="button"
            class="w-full whitespace-nowrap px-3 py-1.5 text-left text-sm hover:bg-surface-200 dark:hover:bg-surface-700 {selectedProjectId === '' ? 'font-medium text-primary-500' : ''}"
            onmousedown={() => { onprojectChange?.(''); projectDropdownOpen = false }}
          >
            All projects
          </button>
          {#each projectStore.list as p (p.id)}
            <button
              type="button"
              class="w-full whitespace-nowrap px-3 py-1.5 text-left text-sm hover:bg-surface-200 dark:hover:bg-surface-700 {selectedProjectId === p.id ? 'font-medium text-primary-500' : ''}"
              onmousedown={() => { onprojectChange?.(p.id); projectDropdownOpen = false }}
            >
              {p.owner}/{p.repo}
            </button>
          {/each}
        </div>
      {/if}
    </div>
  {/if}

  <!-- Tag filter -->
  {#if showTags}
    <div class="relative">
      <div class="flex h-8 flex-wrap items-center gap-1 rounded-md border border-surface-300 bg-surface-50 px-2 dark:border-surface-700 dark:bg-surface-800">
        {#each selectedTags as tag}
          <span class="inline-flex items-center gap-1 rounded bg-primary-500 px-1.5 py-0.5 text-xs font-medium text-white dark:bg-primary-600">
            {tag}
            <button type="button" class="hover:text-primary-200" onclick={() => removeTag(tag)} aria-label="Remove tag {tag}">&times;</button>
          </span>
        {/each}
        <input
          bind:value={tagInput}
          type="text"
          placeholder={selectedTags.length ? '' : 'Filter by tag...'}
          class="min-w-[80px] flex-1 bg-transparent py-0.5 text-sm outline-none"
          onfocus={() => (tagInputFocused = true)}
          onblur={() => setTimeout(() => (tagInputFocused = false), 150)}
          onkeydown={handleTagKeydown}
          data-testid="tag-input"
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
  {/if}

  <!-- Date range -->
  {#if showDateRange}
    <div class="flex items-center gap-2">
      <input
        type="date"
        value={dateFrom}
        oninput={(e) => ondateChange?.((e.currentTarget as HTMLInputElement).value, dateTo)}
        class="h-8 rounded-md border border-surface-300 bg-surface-50 px-2 text-sm dark:border-surface-700 dark:bg-surface-800"
        aria-label="From date"
        data-testid="date-from"
      />
      <span class="text-xs text-surface-400">to</span>
      <input
        type="date"
        value={dateTo}
        oninput={(e) => ondateChange?.(dateFrom, (e.currentTarget as HTMLInputElement).value)}
        class="h-8 rounded-md border border-surface-300 bg-surface-50 px-2 text-sm dark:border-surface-700 dark:bg-surface-800"
        aria-label="To date"
        data-testid="date-to"
      />
    </div>
  {/if}

  {#if onclear && hasActive}
    <button
      type="button"
      class="text-xs text-surface-500 underline hover:text-surface-700 dark:hover:text-surface-300"
      onclick={onclear}
      data-testid="clear-filters"
    >
      Clear filters
    </button>
  {/if}
</div>
