<script lang="ts">
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { renovateStore } from '../stores/renovate.svelte.js'
  import { issueStore } from '../stores/issues.svelte.js'
  import PRCard from '../components/PRCard.svelte'
  import RenovatePRCard from '../components/RenovatePRCard.svelte'
  import IssueCard from '../components/IssueCard.svelte'
  import PRDetailView from '../components/PRDetailView.svelte'
  import {
    ApproveRenovatePR,
    MergeRenovatePR,
    RerunRenovateChecks,
    FixRenovateCI,
  } from '../../wailsjs/go/main/App.js'
  import type { github } from '../../wailsjs/go/models.js'

  type Tab = 'my-prs' | 'reviews' | 'renovate' | 'issues'

  let activeTab = $state<Tab>('my-prs')
  let selectedPR = $state<{ pr: github.PullRequest; checkRuns?: github.CheckRunInfo[]; source: Tab } | null>(null)

  $effect(() => {
    reviewStore.load()
    reviewStore.startPolling()
    renovateStore.load()
    renovateStore.startPolling()
    renovateStore.listen()
    issueStore.load()
    issueStore.startPolling()
    issueStore.listen()
    return () => {
      reviewStore.stopPolling()
      renovateStore.stopPolling()
      renovateStore.stopListening()
      issueStore.stopPolling()
      issueStore.stopListening()
    }
  })

  function selectPR(pr: github.PullRequest, checkRuns?: github.CheckRunInfo[]) {
    selectedPR = { pr, checkRuns, source: activeTab }
  }

  function clearSelection() {
    selectedPR = null
  }

  const tabs: { id: Tab; label: string; count: () => number }[] = [
    { id: 'my-prs', label: 'My PRs', count: () => reviewStore.createdByMe.length },
    { id: 'reviews', label: 'Reviews', count: () => reviewStore.reviewRequested.length },
    { id: 'renovate', label: 'Renovate', count: () => renovateStore.count },
    { id: 'issues', label: 'Issues', count: () => issueStore.count },
  ]

  function prPriority(pr: github.PullRequest): number {
    const ready = !pr.isDraft && pr.mergeable === 'MERGEABLE' &&
      (pr.ciStatus === 'SUCCESS' || pr.ciStatus === '') &&
      (pr.reviewDecision === 'APPROVED' || pr.reviewDecision === '')
    if (ready) return 0 // ready to merge
    if (!pr.viewerHasApproved && pr.reviewDecision !== 'APPROVED') return 1 // to approve
    if (pr.ciStatus === 'FAILURE' || pr.mergeable === 'CONFLICTING') return 2 // to fix
    return 3
  }

  type GroupedPRs<T extends github.PullRequest> = { repo: string; prs: T[] }[]

  function groupByRepo<T extends github.PullRequest>(prs: T[]): GroupedPRs<T> {
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
  const groupedRenovate = $derived(groupByRepo(renovateStore.prs))
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
      {#if renovateStore.loading && renovateStore.count === 0}
        <p class="text-center text-sm opacity-60">Loading...</p>
      {:else if renovateStore.error}
        <p class="text-center text-sm text-error-500">{renovateStore.error}</p>
      {:else if renovateStore.count === 0}
        <p class="py-8 text-center text-sm opacity-50">No Renovate PRs</p>
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
