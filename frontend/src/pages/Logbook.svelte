<script lang="ts">
  import type { Task } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { matchesQuery, matchesProject, matchesTags, matchesDateRange } from '../lib/task-filters.js'
  import { toUtcDayStart, toUtcDayEnd } from '../lib/dates.js'
  import TaskFilterPanel from '../components/TaskFilterPanel.svelte'
  import StatusBadge from '../components/StatusBadge.svelte'

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
    taskStore.list.filter((t: Task) => t.status === 'done' || t.status === 'cancelled'),
  )

  const allTags = $derived(
    [...new Set(logbookTasks.flatMap((t: Task) => t.tags ?? []))].sort(),
  )

  const filteredTasks = $derived.by(() => {
    const from = toUtcDayStart(dateFrom)
    const to = toUtcDayEnd(dateTo)

    return logbookTasks
      .filter((t: Task) => {
        if (selectedStatus !== 'all' && t.status !== selectedStatus) return false
        if (!matchesQuery(t, searchQuery)) return false
        if (!matchesProject(t, selectedProjectId)) return false
        if (!matchesTags(t, selectedTags)) return false
        if (!matchesDateRange(t, from, to, 'closedAt')) return false
        return true
      })
      .sort((a: Task, b: Task) => {
        const aTime = a.closedAt ? new Date(a.closedAt).getTime() : 0
        const bTime = b.closedAt ? new Date(b.closedAt).getTime() : 0
        const diff = sortAsc ? aTime - bTime : bTime - aTime
        if (diff !== 0) return diff
        return a.title.localeCompare(b.title)
      })
  })

  const hasActiveFilters = $derived(
    !!searchQuery
      || selectedStatus !== 'all'
      || !!selectedProjectId
      || selectedTags.length > 0
      || !!dateFrom
      || !!dateTo,
  )

  function clearFilters() {
    searchQuery = ''
    selectedStatus = 'all'
    selectedProjectId = ''
    selectedTags = []
    dateFrom = ''
    dateTo = ''
  }

  function formatClosed(val: unknown): string {
    if (!val) return '—'
    const d = new Date(val as string | number | Date)
    if (isNaN(d.getTime())) return '—'
    return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
  }
</script>

<div class="flex min-h-0 flex-1 flex-col overflow-hidden">
  <!-- Header -->
  <div class="flex items-center gap-3 border-b border-surface-200 px-4 py-3 dark:border-surface-700">
    <h1 class="text-lg font-semibold">Logbook</h1>
    <span class="text-sm text-surface-400">{logbookTasks.length} closed</span>
  </div>

  <!-- Filter bar -->
  <div class="flex flex-wrap items-center gap-2 border-b border-surface-200 px-4 py-2 dark:border-surface-700">
    <TaskFilterPanel
      query={searchQuery}
      onqueryChange={(q) => (searchQuery = q)}
      showProject
      selectedProjectId={selectedProjectId}
      onprojectChange={(id) => (selectedProjectId = id)}
      showDateRange
      dateFrom={dateFrom}
      dateTo={dateTo}
      ondateChange={(f, t) => { dateFrom = f; dateTo = t }}
      statusPills={statusPills.map((p) => ({ val: p.val, label: p.label }))}
      selectedStatus={selectedStatus}
      onstatusChange={(s) => { selectedStatus = s as 'all' | 'done' | 'cancelled' }}
      onclear={clearFilters}
      hasActive={hasActiveFilters}
    />

    <!-- Sort toggle (page-specific, kept inline) -->
    <button
      type="button"
      class="rounded-md border border-surface-300 bg-surface-50 px-2 py-1 text-xs font-medium transition-colors hover:bg-surface-200 dark:border-surface-700 dark:bg-surface-800"
      onclick={() => (sortAsc = !sortAsc)}
      title="Sort by closed date"
    >
      {sortAsc ? '↑ Oldest' : '↓ Newest'}
    </button>
  </div>

  <!-- Tag filter row -->
  {#if allTags.length > 0}
    <div class="flex flex-wrap gap-1.5 border-b border-surface-200 px-4 py-2 dark:border-surface-700">
      {#each allTags as tag}
        <button
          type="button"
          class="rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors {selectedTags.includes(tag)
            ? 'border-primary-500 bg-primary-500 text-white'
            : 'border-surface-300 bg-surface-100 text-surface-600 hover:bg-surface-200 dark:border-surface-600 dark:bg-surface-800 dark:text-surface-300}'}"
          onclick={() =>
            selectedTags.includes(tag)
              ? (selectedTags = selectedTags.filter((t) => t !== tag))
              : (selectedTags = [...selectedTags, tag])}
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
            <tr
              class="cursor-pointer border-b border-surface-100 transition-colors hover:bg-surface-100 dark:border-surface-800 dark:hover:bg-surface-800"
              onclick={() => onviewtask(t.id)}
            >
              <td class="px-4 py-2 font-medium">{t.title}</td>
              <td class="px-4 py-2">
                <StatusBadge status={t.status} />
              </td>
              <td class="hidden px-4 py-2 text-surface-500 md:table-cell">{t.projectId || '—'}</td>
              <td class="hidden px-4 py-2 lg:table-cell">
                <div class="flex flex-wrap gap-1">
                  {#each t.tags ?? [] as tag}
                    <span class="rounded-full bg-surface-200 px-1.5 py-0.5 text-xs dark:bg-surface-700">{tag}</span>
                  {/each}
                </div>
              </td>
              <td class="px-4 py-2 text-xs text-surface-400">{formatClosed(t.closedAt)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
</div>
