<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Props {
    title: string
    description?: string
    children: Snippet
  }

  const { title, description, children }: Props = $props()
</script>

<section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
  <div class="mb-4">
    <h2 class="text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">{title}</h2>
    {#if description}
      <p class="mt-1 text-xs text-surface-500 dark:text-surface-400">{description}</p>
    {/if}
  </div>
  <div class="flex flex-col gap-4">
    {@render children()}
  </div>
</section>
