<script lang="ts">
  import { Search, X } from '@lucide/svelte'
  import MobileSheet from './shell/MobileSheet.svelte'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'

  interface Result {
    type: 'page' | 'task' | 'project'
    id: string
    title: string
    subtitle?: string
  }

  interface Props {
    open: boolean
    onclose: () => void
    onnavigate: (kind: string, params?: { taskId?: string; projectId?: string }) => void
  }

  const { open, onclose, onnavigate }: Props = $props()

  const PAGES: Result[] = [
    { type: 'page', id: 'dashboard', title: 'Dashboard' },
    { type: 'page', id: 'task-list', title: 'Board', subtitle: 'Task Board' },
    { type: 'page', id: 'project-list', title: 'Projects' },
    { type: 'page', id: 'chats', title: 'Chats' },
    { type: 'page', id: 'agents', title: 'Agents' },
    { type: 'page', id: 'github', title: 'GitHub' },
    { type: 'page', id: 'reviews', title: 'Reviews' },
    { type: 'page', id: 'stats', title: 'Stats' },
    { type: 'page', id: 'settings', title: 'Settings' },
    { type: 'page', id: 'workflows', title: 'Workflows' },
  ]

  let query = $state('')
  let selectedIndex = $state(0)
  let inputRef = $state<HTMLInputElement | null>(null)

  const results = $derived.by((): Result[] => {
    const q = query.toLowerCase().trim()
    if (!q) return PAGES.slice(0, 8)

    const matches: Result[] = []

    for (const p of PAGES) {
      if (p.title.toLowerCase().includes(q) || p.subtitle?.toLowerCase().includes(q)) {
        matches.push(p)
      }
    }

    for (const t of taskStore.list) {
      if (matches.length >= 15) break
      if (t.title.toLowerCase().includes(q)) {
        matches.push({ type: 'task', id: t.id, title: t.title, subtitle: t.status })
      }
    }

    for (const p of projectStore.list) {
      const name = `${p.owner}/${p.repo}`
      if (name.toLowerCase().includes(q)) {
        matches.push({ type: 'project', id: p.id, title: name, subtitle: 'Project' })
      }
    }

    return matches.slice(0, 15)
  })

  $effect(() => {
    if (open) {
      query = ''
      selectedIndex = 0
      requestAnimationFrame(() => inputRef?.focus())
    }
  })

  $effect(() => {
    void query
    selectedIndex = 0
  })

  $effect(() => {
    if (!open) return
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        onclose()
      } else if (e.key === 'ArrowDown') {
        e.preventDefault()
        selectedIndex = Math.min(selectedIndex + 1, results.length - 1)
      } else if (e.key === 'ArrowUp') {
        e.preventDefault()
        selectedIndex = Math.max(selectedIndex - 1, 0)
      } else if (e.key === 'Enter') {
        e.preventDefault()
        selectResult(results[selectedIndex])
      }
    }
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })

  function selectResult(result: Result | undefined) {
    if (!result) return
    if (result.type === 'page') {
      onnavigate(result.id)
    } else if (result.type === 'task') {
      onnavigate('task-detail', { taskId: result.id })
    } else if (result.type === 'project') {
      onnavigate('project-detail', { projectId: result.id })
    }
  }

</script>

<MobileSheet {open} onOpenChange={(o) => { if (!o) onclose() }} variant="top">
  <div class="flex flex-col">
    <!-- Search bar -->
    <div class="flex items-center gap-3 border-b border-surface-200 px-4 dark:border-surface-700">
      <Search size={16} class="shrink-0 transition-colors {query ? 'text-primary-500' : 'text-surface-400'}" />
      <input
        bind:this={inputRef}
        bind:value={query}
        type="text"
        placeholder="Search tasks, pages, projects..."
        class="flex-1 bg-transparent py-3.5 text-base outline-none placeholder:text-surface-400 md:text-sm"
      />
      <button
        type="button"
        onclick={onclose}
        class="tap -mr-2 flex items-center gap-1.5 text-surface-400 hover:text-surface-600 dark:hover:text-surface-300"
        aria-label="Close"
      >
        <kbd class="hidden rounded border border-surface-300 px-1.5 py-0.5 font-mono text-[10px] text-surface-400 dark:border-surface-600 md:inline">Esc</kbd>
        <X size={20} class="md:hidden" />
      </button>
    </div>

    <!-- Results -->
    <ul class="max-h-[60dvh] overflow-y-auto py-1 md:max-h-80">
      {#if results.length === 0}
        <li class="px-4 py-8 text-center text-sm text-surface-400">
          No results for "<span class="text-surface-700 dark:text-surface-300">{query}</span>"
        </li>
      {:else}
        {#each results as result, i}
          <li>
            <button
              type="button"
              class="tap flex w-full items-center gap-3 border-l-2 px-3 py-2.5 text-left text-sm transition-colors
                {i === selectedIndex
                  ? 'border-primary-500 bg-primary-50 text-primary-900 dark:bg-primary-950/50 dark:text-primary-100'
                  : 'border-transparent hover:bg-surface-100 dark:hover:bg-surface-800'}"
              onclick={() => selectResult(result)}
              onmouseenter={() => { selectedIndex = i }}
            >
              <span class="shrink-0 rounded px-1.5 py-0.5 text-[10px] font-bold uppercase
                {result.type === 'page'
                  ? 'bg-primary-100 text-primary-700 dark:bg-primary-900/50 dark:text-primary-300'
                  : result.type === 'task'
                    ? 'bg-secondary-100 text-secondary-700 dark:bg-secondary-900/50 dark:text-secondary-300'
                    : 'bg-tertiary-100 text-tertiary-700 dark:bg-tertiary-900/50 dark:text-tertiary-300'}">
                {result.type === 'page' ? 'PAGE' : result.type === 'task' ? 'TASK' : 'PROJ'}
              </span>
              <span class="flex-1 font-medium">{result.title}</span>
              {#if result.subtitle}
                <span class="text-xs text-surface-400">{result.subtitle}</span>
              {/if}
              {#if i === selectedIndex}
                <span class="shrink-0 font-mono text-xs text-primary-500">↵</span>
              {/if}
            </button>
          </li>
        {/each}
      {/if}
    </ul>

    <!-- Footer hints -->
    <div class="hidden items-center gap-4 border-t border-surface-200 px-4 py-2.5 dark:border-surface-700 md:flex">
      <div class="flex gap-4 text-xs text-surface-400">
        <span class="flex items-center gap-1">
          <kbd class="rounded border border-surface-300 px-1 py-0.5 font-mono text-[10px] dark:border-surface-600">↑↓</kbd>
          navigate
        </span>
        <span class="flex items-center gap-1">
          <kbd class="rounded border border-surface-300 px-1 py-0.5 font-mono text-[10px] dark:border-surface-600">↵</kbd>
          open
        </span>
      </div>
    </div>
  </div>
</MobileSheet>
