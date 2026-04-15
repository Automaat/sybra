<script lang="ts">
  import { ChevronRight } from '@lucide/svelte'
  interface Props {
    oldText: string
    newText: string
    filePath?: string
  }

  const { oldText, newText, filePath = '' }: Props = $props()

  let expanded = $state(true)

  const oldLines = $derived(oldText.split('\n'))
  const newLines = $derived(newText.split('\n'))
</script>

<div class="rounded border border-surface-300 text-xs dark:border-surface-600">
  {#if filePath}
    <button
      type="button"
      class="flex w-full items-center gap-2 border-b border-surface-200 px-2 py-1 text-left dark:border-surface-700"
      onclick={() => (expanded = !expanded)}
    >
      <ChevronRight size={12} class="transition-transform {expanded ? 'rotate-90' : ''}" />
      <span class="font-mono text-surface-600 dark:text-surface-400">{filePath}</span>
    </button>
  {/if}

  {#if expanded}
    <div class="overflow-x-auto font-mono">
      {#each oldLines as line, i (i)}
        <div class="flex bg-red-50 text-red-800 dark:bg-red-950 dark:text-red-300">
          <span class="w-8 shrink-0 select-none border-r border-surface-200 px-1 text-right text-surface-400 dark:border-surface-700">{i + 1}</span>
          <span class="px-2">- {line}</span>
        </div>
      {/each}
      {#each newLines as line, i (i)}
        <div class="flex bg-green-50 text-green-800 dark:bg-green-950 dark:text-green-300">
          <span class="w-8 shrink-0 select-none border-r border-surface-200 px-1 text-right text-surface-400 dark:border-surface-700">{i + 1}</span>
          <span class="px-2">+ {line}</span>
        </div>
      {/each}
    </div>
  {/if}
</div>
