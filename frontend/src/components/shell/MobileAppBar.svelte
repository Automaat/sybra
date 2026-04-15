<script lang="ts">
  import { ChevronLeft, Search } from '@lucide/svelte'
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
        <ChevronLeft size={24} />
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
      <Search size={20} />
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
