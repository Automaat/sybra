<script lang="ts">
  import type { Snippet } from 'svelte'
  import { notificationStore } from '../stores/notifications.svelte.js'

  interface Props {
    children?: Snippet
    fallback?: Snippet<[unknown, () => void]>
    onerror?: (error: unknown, reset: () => void) => void
  }

  const { children, fallback, onerror }: Props = $props()

  function handleError(error: unknown, reset: () => void) {
    const message = error instanceof Error ? error.message : String(error)
    notificationStore.pushLocal('error', 'Component error', message)
    onerror?.(error, reset)
  }
</script>

<svelte:boundary onerror={handleError}>
  {#if children}{@render children()}{/if}
  {#snippet failed(error, reset)}
    {#if fallback}
      {@render fallback(error, reset)}
    {:else}
      <div class="rounded-lg border border-error-300 bg-error-50 p-4 text-error-700 dark:border-error-700 dark:bg-error-900/20 dark:text-error-400">
        <p class="text-sm font-medium">Something went wrong</p>
        <p class="mt-1 text-xs opacity-75">{error instanceof Error ? error.message : String(error)}</p>
        <button
          type="button"
          class="mt-2 text-xs underline hover:no-underline"
          onclick={reset}
        >
          Try again
        </button>
      </div>
    {/if}
  {/snippet}
</svelte:boundary>
