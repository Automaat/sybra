<script lang="ts">
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

  function typeIcon(type: 'page' | 'task' | 'project'): string {
    if (type === 'page') return '→'
    if (type === 'task') return '□'
    return '◈'
  }
</script>

<MobileSheet {open} onOpenChange={(o) => { if (!o) onclose() }} variant="top">
  <div class="flex flex-col">
    <div class="flex items-center border-b border-surface-300 px-4 dark:border-surface-600">
      <svg class="h-4 w-4 shrink-0 text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
      </svg>
      <input
        bind:this={inputRef}
        bind:value={query}
        type="text"
        placeholder="Search tasks, pages, projects..."
        class="flex-1 bg-transparent py-3.5 pl-3 text-base outline-none placeholder:text-surface-400 md:text-sm"
      />
      <button
        type="button"
        onclick={onclose}
        class="tap -mr-2 rounded text-surface-400 active:bg-surface-200 dark:active:bg-surface-700"
        aria-label="Close"
      >
        <svg class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
        </svg>
      </button>
    </div>

    <ul class="max-h-[60dvh] overflow-y-auto py-2 md:max-h-80">
      {#if results.length === 0}
        <li class="px-4 py-3 text-sm italic text-surface-400">No results</li>
      {:else}
        {#each results as result, i}
          <li>
            <button
              type="button"
              class="tap flex w-full items-center gap-3 px-4 py-3 text-left text-sm transition-colors {i === selectedIndex ? 'bg-primary-100 text-primary-800 dark:bg-primary-900/40 dark:text-primary-200' : 'active:bg-surface-200 dark:active:bg-surface-700'}"
              onclick={() => selectResult(result)}
              onmouseenter={() => { selectedIndex = i }}
            >
              <span class="w-4 shrink-0 text-center font-mono text-xs text-surface-400">{typeIcon(result.type)}</span>
              <span class="flex-1 font-medium">{result.title}</span>
              {#if result.subtitle}
                <span class="text-xs text-surface-400">{result.subtitle}</span>
              {/if}
            </button>
          </li>
        {/each}
      {/if}
    </ul>

    <div class="hidden border-t border-surface-300 px-4 py-2 dark:border-surface-600 md:block">
      <div class="flex gap-4 text-xs text-surface-400">
        <span><kbd class="font-mono">↑↓</kbd> navigate</span>
        <span><kbd class="font-mono">↵</kbd> open</span>
        <span><kbd class="font-mono">Esc</kbd> close</span>
      </div>
    </div>
  </div>
</MobileSheet>
