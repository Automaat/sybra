<script lang="ts">
  import { Search, X } from '@lucide/svelte'
  import type { task } from '../../wailsjs/go/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { projectStore } from '../stores/projects.svelte.js'
  import { STATUS_MAP } from '../lib/statuses.js'

  interface Props {
    onviewtask: (id: string) => void
  }

  const { onviewtask }: Props = $props()

  // Filter state
  let searchQuery = $state('')
  let selectedStatus = $state<'all' | 'done' | 'cancelled'>('all')
  let selectedProjectId = $state('')
  let selectedTags = $state<string[]>([])
  let dateFrom = $state('')
  let dateTo = $state('')
  let sortAsc = $state(false)

  const statusPills: { val: 'all' | 'done' | 'cancelled'; label: string }[] = [
    { val: 'all', label: 'All' },
    { val: 'done', label: 'Done' },
    { val: 'cancelled', label: 'Cancelled' },
  ]

  const logbookTasks = $derived(
    taskStore.list.filter((t: task.Task) => t.status === 'done' || t.status === 'cancelled')
  )

  const allTags = $derived(
    [...new Set(logbookTasks.flatMap((t: task.Task) => t.tags ?? []))].sort()
  )

  function toUtcDayStart(dateStr: string): Date | null {
    if (!dateStr) return null
    const [y, m, d] = dateStr.split('-').map(Number)
    return new Date(Date.UTC(y, m - 1, d, 0, 0, 0, 0))
  }

  function toUtcDayEnd(dateStr: string): Date | null {
    if (!dateStr) return null
    const [y, m, d] = dateStr.split('-').map(Number)
    return new Date(Date.UTC(y, m - 1, d, 23, 59, 59, 999))
  }

  const filteredTasks = $derived.by(() => {
    const query = searchQuery.toLowerCase().trim()
    const from = toUtcDayStart(dateFrom)
    const to = toUtcDayEnd(dateTo)

    return logbookTasks
      .filter((t: task.Task) => {
        if (selectedStatus !== 'all' && t.status !== selectedStatus) return false
        if (query && !t.title.toLowerCase().includes(query)
            && !(t.body ?? '').toLowerCase().includes(query)) return false
        if (selectedProjectId && t.projectId !== selectedProjectId) return false
        if (selectedTags.length > 0 && !selectedTags.every(tag => t.tags?.includes(tag))) return false
        if (from || to) {
          const closed = t.closedAt ? new Date(t.closedAt) : null
          if (!closed) return false
          if (from && closed < from) return false
          if (to && closed > to) return false
        }
        return true
      })
      .sort((a: task.Task, b: task.Task) => {
        const aTime = a.closedAt ? new Date(a.closedAt).getTime() : 0
        const bTime = b.closedAt ? new Date(b.closedAt).getTime() : 0
        const diff = sortAsc ? aTime - bTime : bTime - aTime
        if (diff !== 0) return diff
        return a.title.localeCompare(b.title)
      })
  })

  const hasActiveFilters = $derived(
    searchQuery || selectedStatus !== 'all' || selectedProjectId || selectedTags.length > 0 || dateFrom || dateTo
  )

  function clearFilters() {
    searchQuery = ''
    selectedStatus = 'all'
    selectedProjectId = ''
    selectedTags = []
    dateFrom = ''
    dateTo = ''
  }

  function removeTag(tag: string) {
    selectedTags = selectedTags.filter(t => t !== tag)
  }

  function formatDate(val: any): string {
    if (!val) return '—'
    const d = new Date(val)
    if (isNaN(d.getTime())) return '—'
    return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
  }

  const projects = $derived(projectStore.list)
</script>

<div class="flex min-h-0 flex-1 flex-col overflow-hidden">
  <!-- Header -->
  <div class="flex items-center gap-3 border-b border-surface-200 px-4 py-3 dark:border-surface-700">
    <h1 class="text-lg font-semibold">Logbook</h1>
    <span class="text-sm text-surface-400">{logbookTasks.length} closed</span>
  </div>

  <!-- Filter bar -->
  <div class="flex flex-wrap items-center gap-2 border-b border-surface-200 px-4 py-2 dark:border-surface-700">
    <!-- Search -->
    <div class="relative flex items-center">
      <Search size={14} class="absolute left-2 text-surface-400" />
      <input
        type="search"
        placeholder="Search…"
        bind:value={searchQuery}
        class="h-7 rounded-md border border-surface-300 bg-surface-50 pl-7 pr-2 text-xs focus:outline-none focus:ring-1 focus:ring-primary-400 dark:border-surface-700 dark:bg-surface-800"
      />
    </div>

    <!-- Status filter pills -->
    <div class="flex gap-1">
      {#each statusPills as pill}
        <button
          type="button"
          class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {selectedStatus === pill.val ? 'bg-primary-500 text-white' : 'bg-surface-100 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300'}"
          onclick={() => { selectedStatus = pill.val }}
        >
          {pill.label}
        </button>
      {/each}
    </div>

    <!-- Project filter -->
    {#if projects.length > 0}
      <select
        bind:value={selectedProjectId}
        class="h-7 rounded-md border border-surface-300 bg-surface-50 px-2 text-xs focus:outline-none dark:border-surface-700 dark:bg-surface-800"
      >
        <option value="">All projects</option>
        {#each projects as p}
          <option value={p.id}>{p.owner}/{p.repo}</option>
        {/each}
      </select>
    {/if}

    <!-- Date range -->
    <div class="flex items-center gap-1">
      <input
        type="date"
        bind:value={dateFrom}
        class="h-7 rounded-md border border-surface-300 bg-surface-50 px-2 text-xs focus:outline-none dark:border-surface-700 dark:bg-surface-800"
        title="Closed from"
      />
      <span class="text-xs text-surface-400">–</span>
      <input
        type="date"
        bind:value={dateTo}
        class="h-7 rounded-md border border-surface-300 bg-surface-50 px-2 text-xs focus:outline-none dark:border-surface-700 dark:bg-surface-800"
        title="Closed until"
      />
    </div>

    <!-- Sort toggle -->
    <button
      type="button"
      class="rounded-md border border-surface-300 bg-surface-50 px-2 py-1 text-xs font-medium transition-colors hover:bg-surface-200 dark:border-surface-700 dark:bg-surface-800"
      onclick={() => { sortAsc = !sortAsc }}
      title="Sort by closed date"
    >
      {sortAsc ? '↑ Oldest' : '↓ Newest'}
    </button>

    {#if hasActiveFilters}
      <button
        type="button"
        class="flex items-center gap-1 text-xs text-surface-500 underline hover:text-surface-700 dark:hover:text-surface-300"
        onclick={clearFilters}
      >
        <X size={12} />
        Clear
      </button>
    {/if}
  </div>

  <!-- Tag filter row -->
  {#if allTags.length > 0}
    <div class="flex flex-wrap gap-1.5 border-b border-surface-200 px-4 py-2 dark:border-surface-700">
      {#each allTags as tag}
        <button
          type="button"
          class="rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors {selectedTags.includes(tag)
            ? 'border-primary-500 bg-primary-500 text-white'
            : 'border-surface-300 bg-surface-100 text-surface-600 hover:bg-surface-200 dark:border-surface-600 dark:bg-surface-800 dark:text-surface-300'}"
          onclick={() => selectedTags.includes(tag) ? removeTag(tag) : selectedTags = [...selectedTags, tag]}
        >
          {tag}
        </button>
      {/each}
    </div>
  {/if}

  <!-- Task list -->
  <div class="min-h-0 flex-1 overflow-y-auto">
    {#if taskStore.loading}
      <p class="m-auto p-8 text-center text-sm opacity-60">Loading…</p>
    {:else if logbookTasks.length === 0}
      <div class="flex flex-col items-center gap-2 p-12 text-center text-surface-400">
        <p class="text-sm font-medium">Nothing in the logbook yet</p>
        <p class="text-xs">Completed and cancelled tasks will appear here.</p>
      </div>
    {:else if filteredTasks.length === 0}
      <div class="flex flex-col items-center gap-2 p-12 text-center text-surface-400">
        <p class="text-sm font-medium">No tasks match these filters</p>
        <button type="button" class="text-xs underline" onclick={clearFilters}>Clear filters</button>
      </div>
    {:else}
      <table class="w-full text-sm">
        <thead class="sticky top-0 z-10 border-b border-surface-200 bg-surface-100 dark:border-surface-700 dark:bg-surface-900">
          <tr>
            <th class="px-4 py-2 text-left text-xs font-semibold uppercase tracking-wider text-surface-500">Title</th>
            <th class="px-4 py-2 text-left text-xs font-semibold uppercase tracking-wider text-surface-500">Status</th>
            <th class="hidden px-4 py-2 text-left text-xs font-semibold uppercase tracking-wider text-surface-500 md:table-cell">Project</th>
            <th class="hidden px-4 py-2 text-left text-xs font-semibold uppercase tracking-wider text-surface-500 lg:table-cell">Tags</th>
            <th class="px-4 py-2 text-left text-xs font-semibold uppercase tracking-wider text-surface-500">Closed</th>
          </tr>
        </thead>
        <tbody>
          {#each filteredTasks as t (t.id)}
            {@const meta = STATUS_MAP[t.status]}
            <tr
              class="cursor-pointer border-b border-surface-100 transition-colors hover:bg-surface-100 dark:border-surface-800 dark:hover:bg-surface-800"
              onclick={() => onviewtask(t.id)}
            >
              <td class="px-4 py-2 font-medium">{t.title}</td>
              <td class="px-4 py-2">
                {#if meta}
                  <span class="rounded-full px-2 py-0.5 text-xs font-semibold {meta.badgeClasses}">{meta.label}</span>
                {:else}
                  <span class="rounded-full px-2 py-0.5 text-xs font-semibold bg-surface-200 dark:bg-surface-700">{t.status}</span>
                {/if}
              </td>
              <td class="hidden px-4 py-2 text-surface-500 md:table-cell">{t.projectId || '—'}</td>
              <td class="hidden px-4 py-2 lg:table-cell">
                <div class="flex flex-wrap gap-1">
                  {#each t.tags ?? [] as tag}
                    <span class="rounded-full bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">{tag}</span>
                  {/each}
                </div>
              </td>
              <td class="px-4 py-2 text-xs text-surface-400">{formatDate(t.closedAt)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
</div>
