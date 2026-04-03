<script lang="ts">
  import { reviewStore } from '../stores/reviews.svelte.js'
  import PRCard from '../components/PRCard.svelte'

  $effect(() => {
    reviewStore.load()
    reviewStore.startPolling()
    return () => reviewStore.stopPolling()
  })
</script>

<div class="flex flex-col gap-6 p-6">
  <div class="flex items-center justify-between">
    <p class="text-sm opacity-60">
      {reviewStore.totalCount} pull request{reviewStore.totalCount !== 1 ? 's' : ''}
    </p>
    <button
      type="button"
      class="rounded-lg bg-surface-200 px-3 py-1.5 text-sm font-medium hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
      onclick={() => reviewStore.load()}
    >
      Refresh
    </button>
  </div>

  {#if reviewStore.loading && reviewStore.totalCount === 0}
    <p class="text-center text-sm opacity-60">Loading pull requests...</p>
  {:else if reviewStore.error}
    <p class="text-center text-sm text-error-500">{reviewStore.error}</p>
  {:else}
    <section class="flex flex-col gap-2">
      <h2 class="flex items-center gap-2 text-sm font-semibold uppercase tracking-wide opacity-70">
        Review Requested
        <span class="rounded-full bg-primary-500 px-2 py-0.5 text-xs font-bold text-white">
          {reviewStore.reviewRequested.length}
        </span>
      </h2>
      {#if reviewStore.reviewRequested.length === 0}
        <p class="py-4 text-center text-sm opacity-50">No pending review requests</p>
      {:else}
        {#each reviewStore.reviewRequested as pr (pr.url)}
          <PRCard {pr} />
        {/each}
      {/if}
    </section>

    <section class="flex flex-col gap-2">
      <h2 class="flex items-center gap-2 text-sm font-semibold uppercase tracking-wide opacity-70">
        My PRs
        <span class="rounded-full bg-surface-400 px-2 py-0.5 text-xs font-bold text-white dark:bg-surface-500">
          {reviewStore.createdByMe.length}
        </span>
      </h2>
      {#if reviewStore.createdByMe.length === 0}
        <p class="py-4 text-center text-sm opacity-50">No open pull requests</p>
      {:else}
        {#each reviewStore.createdByMe as pr (pr.url)}
          <PRCard {pr} />
        {/each}
      {/if}
    </section>
  {/if}
</div>
