<script lang="ts">
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { renovateStore } from '../stores/renovate.svelte.js'
  import { issueStore } from '../stores/issues.svelte.js'
  import PRCard from '../components/PRCard.svelte'
  import RenovatePRCard from '../components/RenovatePRCard.svelte'
  import IssueCard from '../components/IssueCard.svelte'
  import PRDetailView from '../components/PRDetailView.svelte'
  import { Search } from '@lucide/svelte'
  import {
    ApproveRenovatePR,
    MergeRenovatePR,
    RerunRenovateChecks,
    FixRenovateCI,
  } from '$lib/api'
  import { isRenovatePRReadyToMerge } from '$lib/renovate.js'
  import type { CheckRunInfo, PullRequest, RenovatePR } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

  type Tab = 'my-prs' | 'reviews' | 'renovate' | 'issues'

  let activeTab = $state<Tab>('my-prs')
  let selectedPR = $state<{ pr: PullRequest; checkRuns?: CheckRunInfo[]; source: Tab } | null>(null)
  let renovateQuery = $state('')
  let renovateSearchRef = $state<HTMLInputElement | null>(null)

  $effect(() => {
    reviewStore.load()
    reviewStore.startPolling()
    renovateStore.load()
    renovateStore.startPolling()
    renovateStore.listen()
    issueStore.load()
    issueStore.startPolling()
    issueStore.listen()

    function onFocusRenovateSearch() {
      activeTab = 'renovate'
      requestAnimationFrame(() => {
        renovateSearchRef?.focus()
        renovateSearchRef?.select()
      })
    }
    window.addEventListener('focus-renovate-search', onFocusRenovateSearch)

    return () => {
      reviewStore.stopPolling()
      renovateStore.stopPolling()
      renovateStore.stopListening()
      issueStore.stopPolling()
      issueStore.stopListening()
      window.removeEventListener('focus-renovate-search', onFocusRenovateSearch)
    }
  })

  function matchesRenovate(pr: RenovatePR, q: string): boolean {
    return (
      pr.repository.toLowerCase().includes(q) ||
      pr.title.toLowerCase().includes(q) ||
      pr.labels.some((l) => l.toLowerCase().includes(q))
    )
  }

  function onSearchKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault()
      renovateQuery = ''
      renovateSearchRef?.blur()
    }
  }

  function selectPR(pr: PullRequest, checkRuns?: CheckRunInfo[]) {
    selectedPR = { pr, checkRuns, source: activeTab }
  }

  function clearSelection() {
    selectedPR = null
  }

  const tabs: { id: Tab; label: string; count: () => number }[] = [
    { id: 'my-prs', label: 'My PRs', count: () => reviewStore.createdByMe.length },
    { id: 'reviews', label: 'To Review', count: () => reviewStore.reviewRequested.length },
    { id: 'renovate', label: 'Renovate', count: () => renovateStore.count },
    { id: 'issues', label: 'Issues', count: () => issueStore.count },
  ]

  function prPriority(pr: PullRequest & { waitingForStability?: boolean }): number {
    const ready = isRenovatePRReadyToMerge(pr)
    if (ready) return 0 // ready to merge
    if (!pr.viewerHasApproved && pr.reviewDecision !== 'APPROVED') return 1 // to approve
    if (pr.ciStatus === 'FAILURE' || pr.mergeable === 'CONFLICTING') return 2 // to fix
    return 3
  }

  type GroupedPRs<T extends PullRequest> = { repo: string; prs: T[] }[]

  function groupByRepo<T extends PullRequest>(prs: T[]): GroupedPRs<T> {
    const sorted = [...prs].sort((a, b) => prPriority(a) - prPriority(b))
    const groups = new Map<string, T[]>()
    for (const pr of sorted) {
      const list = groups.get(pr.repository)
      if (list) list.push(pr)
      else groups.set(pr.repository, [pr])
    }
    return Array.from(groups, ([repo, prs]) => ({ repo, prs }))
  }

  const groupedMyPRs = $derived(groupByRepo(reviewStore.createdByMe))
  const groupedReviews = $derived(groupByRepo(reviewStore.reviewRequested))
  const filteredRenovate = $derived.by(() => {
    const q = renovateQuery.trim().toLowerCase()
    if (!q) return renovateStore.prs
    return renovateStore.prs.filter((pr) => matchesRenovate(pr, q))
  })
  const groupedRenovate = $derived(groupByRepo(filteredRenovate))
</script>

{#if selectedPR}
  <PRDetailView
    pr={selectedPR.pr}
    checkRuns={selectedPR.checkRuns}
    onback={clearSelection}
    onapprove={selectedPR.source === 'renovate' ? async () => {
      await ApproveRenovatePR(selectedPR!.pr.repository, selectedPR!.pr.number)
      await renovateStore.load()
      clearSelection()
    } : undefined}
    onmerge={selectedPR.source === 'renovate' ? async () => {
      await MergeRenovatePR(selectedPR!.pr.repository, selectedPR!.pr.number)
      await renovateStore.load()
      clearSelection()
    } : undefined}
    onrerun={selectedPR.source === 'renovate' ? async () => {
      await RerunRenovateChecks(selectedPR!.pr.repository, selectedPR!.pr.number)
      await renovateStore.load()
      clearSelection()
    } : undefined}
    onfix={selectedPR.source === 'renovate' ? async () => {
      await FixRenovateCI(selectedPR!.pr.repository, selectedPR!.pr.number, selectedPR!.pr.headRefName, selectedPR!.pr.title)
      await renovateStore.load()
      clearSelection()
    } : undefined}
  />
{:else}
  <div class="flex flex-col gap-4 p-6">
    <div class="flex items-center justify-between">
      <div class="flex gap-1 rounded-lg bg-surface-200 p-1 dark:bg-surface-700">
        {#each tabs as tab (tab.id)}
          <button
            type="button"
            class="rounded-md px-3 py-1.5 text-sm font-medium transition-colors
              {activeTab === tab.id
                ? 'bg-white text-surface-900 shadow-sm dark:bg-surface-600 dark:text-white'
                : 'text-surface-500 hover:text-surface-700 dark:hover:text-surface-300'}"
            onclick={() => (activeTab = tab.id)}
          >
            {tab.label}
            {#if tab.count() > 0}
              <span class="ml-1 rounded-full bg-surface-300 px-1.5 py-0.5 text-xs dark:bg-surface-500">
                {tab.count()}
              </span>
            {/if}
          </button>
        {/each}
      </div>

      <button
        type="button"
        class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
        onclick={() => {
          if (activeTab === 'renovate') renovateStore.load()
          else if (activeTab === 'issues') issueStore.load()
          else reviewStore.load()
        }}
      >
        Refresh
      </button>
    </div>

    {#if activeTab === 'my-prs'}
      {#if reviewStore.loading && reviewStore.createdByMe.length === 0}
        <p class="text-center text-sm opacity-60">Loading...</p>
      {:else if reviewStore.createdByMe.length === 0}
        <p class="py-8 text-center text-sm opacity-50">No open pull requests</p>
      {:else}
        {#each groupedMyPRs as group (group.repo)}
          <div class="flex flex-col gap-2">
            <h3 class="text-xs font-semibold uppercase tracking-wide text-surface-400">{group.repo}</h3>
            {#each group.prs as pr (pr.url)}
              <PRCard {pr} onselect={() => selectPR(pr)} />
            {/each}
          </div>
        {/each}
      {/if}

    {:else if activeTab === 'reviews'}
      {#if reviewStore.loading && reviewStore.reviewRequested.length === 0}
        <p class="text-center text-sm opacity-60">Loading...</p>
      {:else if reviewStore.reviewRequested.length === 0}
        <p class="py-8 text-center text-sm opacity-50">No pending review requests</p>
      {:else}
        {#each groupedReviews as group (group.repo)}
          <div class="flex flex-col gap-2">
            <h3 class="text-xs font-semibold uppercase tracking-wide text-surface-400">{group.repo}</h3>
            {#each group.prs as pr (pr.url)}
              <PRCard {pr} onselect={() => selectPR(pr)} />
            {/each}
          </div>
        {/each}
      {/if}

    {:else if activeTab === 'renovate'}
      <div class="relative w-full max-w-md">
        <Search size={16} class="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-surface-400" />
        <input
          bind:this={renovateSearchRef}
          bind:value={renovateQuery}
          onkeydown={onSearchKeydown}
          type="text"
          placeholder="Search by repo, title, or label…"
          class="h-8 w-full rounded-md border border-surface-300 bg-surface-50 pl-8 pr-2 text-sm outline-none focus:border-primary-400 focus:ring-1 focus:ring-primary-400 dark:border-surface-700 dark:bg-surface-800 dark:focus:border-primary-500 dark:focus:ring-primary-500"
        />
      </div>
      {#if renovateStore.loading && renovateStore.count === 0}
        <p class="text-center text-sm opacity-60">Loading...</p>
      {:else if renovateStore.error}
        <div class="flex flex-col items-center gap-2 py-8">
          <p class="text-center text-sm text-error-500">Failed to load Renovate PRs</p>
          <p class="max-w-lg break-words text-center text-xs opacity-70">{renovateStore.error}</p>
          <button
            type="button"
            class="btn btn-sm preset-tonal"
            onclick={() => renovateStore.load()}
          >
            Retry
          </button>
        </div>
      {:else if renovateStore.count === 0}
        <p class="py-8 text-center text-sm opacity-50">No Renovate PRs</p>
      {:else if filteredRenovate.length === 0}
        <p class="py-8 text-center text-sm opacity-50">No matches for "{renovateQuery}"</p>
      {:else}
        {#each groupedRenovate as group (group.repo)}
          <div class="flex flex-col gap-2">
            <h3 class="text-xs font-semibold uppercase tracking-wide text-surface-400">{group.repo}</h3>
            {#each group.prs as pr (pr.url)}
              <RenovatePRCard {pr} onselect={() => selectPR(pr, pr.checkRuns)} />
            {/each}
          </div>
        {/each}
      {/if}

    {:else if activeTab === 'issues'}
      {#if issueStore.loading && issueStore.count === 0}
        <p class="text-center text-sm opacity-60">Loading...</p>
      {:else if issueStore.error}
        <p class="text-center text-sm text-error-500">{issueStore.error}</p>
      {:else if issueStore.count === 0}
        <p class="py-8 text-center text-sm opacity-50">No assigned issues</p>
      {:else}
        <div class="flex flex-col gap-2">
          {#each issueStore.issues as issue (issue.url)}
            <IssueCard {issue} />
          {/each}
        </div>
      {/if}
    {/if}
  </div>
{/if}
