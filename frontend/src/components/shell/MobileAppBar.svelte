<script lang="ts">
  import { navStore } from '../../lib/navigation.svelte.js'

  interface PrimaryAction {
    label: string
    run: () => void
  }

  interface Props {
    onsearch: () => void
    primaryAction?: PrimaryAction | null
  }

  const { onsearch, primaryAction }: Props = $props()
</script>

<header
  class="sticky top-0 z-30 flex shrink-0 items-center gap-2 border-b border-surface-200 bg-surface-50/95 px-3 pt-safe backdrop-blur dark:border-surface-800 dark:bg-surface-950/95"
>
  <div class="flex min-h-[3rem] w-full items-center gap-1">
    {#if navStore.canGoBack}
      <button
        type="button"
        onclick={() => navStore.back()}
        class="tap -ml-2 flex items-center justify-center rounded-lg text-surface-600 active:bg-surface-200 dark:text-surface-300 dark:active:bg-surface-800"
        aria-label="Back"
      >
        <svg class="h-6 w-6" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" d="M15 19l-7-7 7-7" />
        </svg>
      </button>
    {:else}
      <span class="px-2 text-base font-bold text-primary-600 dark:text-primary-400">S</span>
    {/if}

    <h1 class="flex-1 truncate text-base font-semibold">{navStore.pageTitle}</h1>

    <button
      type="button"
      onclick={onsearch}
      class="tap flex items-center justify-center rounded-lg text-surface-600 active:bg-surface-200 dark:text-surface-300 dark:active:bg-surface-800"
      aria-label="Search"
    >
      <svg class="h-5 w-5" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
        <path stroke-linecap="round" stroke-linejoin="round" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z" />
      </svg>
    </button>

    {#if primaryAction}
      <button
        type="button"
        onclick={primaryAction.run}
        class="tap rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white active:bg-primary-700"
        aria-label={primaryAction.label}
      >
        + {primaryAction.label}
      </button>
    {/if}
  </div>
</header>
