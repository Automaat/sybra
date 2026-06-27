<script lang="ts">
  import { ChevronDown, ChevronRight, Folder, GitPullRequest, ListChecks, Search } from '@lucide/svelte'
  import { projectStore } from '../stores/projects.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { BOARD_COLUMNS } from '../lib/statuses.js'
  import { formatShortDate, timeAgo } from '../lib/dates.js'
  import type { Project } from '../../bindings/github.com/Automaat/sybra/internal/project/models.js'

  interface Props {
    onselect: (id: string) => void
    onadd: () => void
  }

  const { onselect, onadd }: Props = $props()

  // "Active" = sits in an active board column (matches Project Detail's board
  // count); terminal and unknown/legacy statuses are excluded.
  const ACTIVE_STATUSES = new Set<string>(BOARD_COLUMNS.flatMap((c) => [c.status, ...c.includes]))

  function activeTaskCount(projectId: string): number {
    return taskStore.list.filter((t) => t.projectId === projectId && ACTIVE_STATUSES.has(t.status)).length
  }

  function openPRCount(owner: string, repo: string): number {
    return reviewStore.byRepo(`${owner}/${repo}`).length
  }

  /** Most recent task activity for a project, as a relative label (empty if none). */
  function lastActivity(projectId: string): string {
    let latestMs = 0
    let latest = ''
    for (const t of taskStore.list) {
      if (t.projectId !== projectId || !t.updatedAt) continue
      const ms = Date.parse(t.updatedAt)
      if (!Number.isNaN(ms) && ms > latestMs) {
        latestMs = ms
        latest = t.updatedAt
      }
    }
    return latest ? timeAgo(latest) : ''
  }

  // pet first, then work — matches the ProjectType enum ordering.
  const TYPE_ORDER = ['pet', 'work'] as const
  const TYPE_LABELS: Record<string, string> = { pet: 'Pet', work: 'Work' }

  let query = $state('')
  // Collapsed group keys: a type ("pet") or a type+owner ("pet/Automaat").
  let collapsed = $state(new Set<string>())

  function toggle(key: string) {
    const next = new Set(collapsed)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    collapsed = next
  }

  interface OwnerGroup {
    owner: string
    projects: Project[]
  }
  interface TypeGroup {
    type: string
    owners: OwnerGroup[]
    count: number
  }

  function matchesQuery(p: Project, q: string): boolean {
    if (!q) return true
    return (
      `${p.owner}/${p.repo}`.toLowerCase().includes(q) ||
      (p.name ?? '').toLowerCase().includes(q) ||
      (p.url ?? '').toLowerCase().includes(q)
    )
  }

  // Two-level grouping: project type → GitHub owner → projects.
  const groups = $derived.by<TypeGroup[]>(() => {
    const q = query.trim().toLowerCase()
    const byType = new Map<string, Map<string, Project[]>>()
    for (const p of projectStore.list) {
      if (!matchesQuery(p, q)) continue
      const type = p.type === 'work' ? 'work' : 'pet'
      let owners = byType.get(type)
      if (!owners) {
        owners = new Map()
        byType.set(type, owners)
      }
      const ownerKey = p.owner || '—'
      const list = owners.get(ownerKey)
      if (list) list.push(p)
      else owners.set(ownerKey, [p])
    }

    return TYPE_ORDER.filter((t) => byType.has(t)).map((type) => {
      const ownersMap = byType.get(type)!
      const owners = [...ownersMap.entries()]
        .sort((a, b) => a[0].toLowerCase().localeCompare(b[0].toLowerCase()))
        .map(([owner, projects]) => ({
          owner,
          projects: projects.sort((a, b) => a.repo.toLowerCase().localeCompare(b.repo.toLowerCase())),
        }))
      const count = owners.reduce((n, o) => n + o.projects.length, 0)
      return { type, owners, count }
    })
  })

  const totalMatches = $derived(groups.reduce((n, g) => n + g.count, 0))
</script>

<div class="flex flex-col gap-4 p-4 md:gap-5 md:p-6">
  {#if projectStore.loading && projectStore.list.length === 0}
    <p class="text-sm opacity-60">Loading projects...</p>
  {:else if projectStore.error}
    <p class="text-sm text-error-500">{projectStore.error}</p>
  {:else if projectStore.list.length === 0}
    <div class="flex flex-col items-center gap-3 py-16 text-center">
      <Folder size={48} class="text-surface-400" />
      <p class="text-sm text-surface-500">No projects yet</p>
      <button
        type="button"
        class="rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600"
        onclick={onadd}
      >
        Add your first project
      </button>
    </div>
  {:else}
    <div class="relative w-full max-w-sm">
      <Search size={16} class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-surface-400" />
      <input
        bind:value={query}
        type="text"
        placeholder="Search projects..."
        class="h-9 w-full rounded-md border border-surface-300 bg-surface-50 pl-8 pr-2 text-sm outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-700 dark:bg-surface-800 dark:focus:border-primary-500 dark:focus:ring-primary-500"
        data-testid="project-search"
      />
    </div>

    {#if totalMatches === 0}
      <p class="text-sm text-surface-500">No projects match “{query}”.</p>
    {:else}
      <div class="flex flex-col gap-6">
        {#each groups as g (g.type)}
          {@const typeCollapsed = collapsed.has(g.type)}
          <section class="flex flex-col gap-3">
            <button
              type="button"
              class="flex items-center gap-2 text-left"
              onclick={() => toggle(g.type)}
            >
              {#if typeCollapsed}
                <ChevronRight size={16} class="text-surface-400" />
              {:else}
                <ChevronDown size={16} class="text-surface-400" />
              {/if}
              <span class="text-xs font-semibold uppercase tracking-wide text-surface-500">{TYPE_LABELS[g.type]}</span>
              <span class="rounded-full bg-surface-200 px-1.5 py-0.5 text-xs text-surface-500 dark:bg-surface-700 dark:text-surface-400">{g.count}</span>
            </button>

            {#if !typeCollapsed}
              <div class="flex flex-col gap-3 pl-1.5">
                {#each g.owners as o (o.owner)}
                  {@const ownerKey = `${g.type}/${o.owner}`}
                  {@const ownerCollapsed = collapsed.has(ownerKey)}
                  <div class="flex flex-col gap-1.5">
                    <button
                      type="button"
                      class="flex items-center gap-1.5 text-left"
                      onclick={() => toggle(ownerKey)}
                    >
                      {#if ownerCollapsed}
                        <ChevronRight size={14} class="text-surface-400" />
                      {:else}
                        <ChevronDown size={14} class="text-surface-400" />
                      {/if}
                      <span class="text-sm font-medium">{o.owner}</span>
                      <span class="text-xs text-surface-400">{o.projects.length}</span>
                    </button>

                    {#if !ownerCollapsed}
                      <div class="overflow-hidden rounded-lg border border-surface-300 divide-y divide-surface-200 dark:border-surface-600 dark:divide-surface-700">
                        {#each o.projects as p (p.id)}
                          {@const active = activeTaskCount(p.id)}
                          {@const prs = openPRCount(p.owner, p.repo)}
                          {@const activity = lastActivity(p.id)}
                          <button
                            type="button"
                            class="flex w-full items-center gap-2 bg-surface-50 px-3 py-2 text-left transition-colors hover:bg-surface-100 dark:bg-surface-800 dark:hover:bg-surface-700"
                            onclick={() => onselect(p.id)}
                          >
                            <Folder size={16} class="shrink-0 text-surface-400" />
                            <span class="truncate text-sm font-medium">{p.repo}</span>
                            {#if p.status === 'cloning'}
                              <span class="shrink-0 rounded px-1.5 py-0.5 text-xs font-medium bg-primary-100 text-primary-700 dark:bg-primary-900/40 dark:text-primary-300">cloning…</span>
                            {:else if p.status === 'error'}
                              <span class="shrink-0 rounded px-1.5 py-0.5 text-xs font-medium bg-error-100 text-error-700 dark:bg-error-900/40 dark:text-error-300">error</span>
                            {/if}
                            <div class="ml-auto flex shrink-0 flex-wrap items-center justify-end gap-x-3 gap-y-0.5 text-xs text-surface-500">
                              <span class="inline-flex items-center gap-1" title="Active tasks">
                                <ListChecks size={13} class="shrink-0" />
                                {active} active
                              </span>
                              {#if prs > 0}
                                <span class="inline-flex items-center gap-1" title="Open pull requests">
                                  <GitPullRequest size={13} class="shrink-0" />
                                  {prs} PR{prs === 1 ? '' : 's'}
                                </span>
                              {/if}
                              {#if activity}
                                <span title="Last task activity">{activity}</span>
                              {/if}
                              <span class="opacity-70" title="Date added">Added {formatShortDate(p.createdAt)}</span>
                            </div>
                          </button>
                        {/each}
                      </div>
                    {/if}
                  </div>
                {/each}
              </div>
            {/if}
          </section>
        {/each}
      </div>
    {/if}
  {/if}
</div>
