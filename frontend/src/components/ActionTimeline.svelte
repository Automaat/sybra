<script lang="ts">
  import type { TimelineEntry } from '$lib/timeline.js'

  interface Props {
    entries: TimelineEntry[]
    activeIndex: number | null
    onselect: (index: number) => void
  }

  const { entries, activeIndex, onselect }: Props = $props()

  let container: HTMLDivElement | undefined = $state()

  const typeStyles: Record<string, string> = {
    init: 'bg-surface-400 dark:bg-surface-500',
    system: 'bg-surface-400 dark:bg-surface-500',
    assistant: 'bg-primary-500',
    tool_use: 'bg-secondary-500',
    tool_result: 'bg-tertiary-500',
    result: 'bg-warning-500',
    user_input: 'bg-primary-400',
    user: 'bg-tertiary-500',
  }

  function dotColor(type: string): string {
    return typeStyles[type] ?? 'bg-surface-400'
  }

  function formatTime(date: Date): string {
    try {
      const d = new Date(date)
      return d.toLocaleTimeString('en-US', {
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      })
    } catch {
      return '--:--:--'
    }
  }

  function onkeydown(e: KeyboardEvent, index: number) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      onselect(index)
    }
  }

  // Auto-scroll to keep active entry visible
  $effect(() => {
    if (activeIndex === null || !container) return
    const el = container.querySelector(`[data-timeline-index="${activeIndex}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  })
</script>

<div class="flex h-full flex-col">
  <div class="shrink-0 border-b border-surface-300 px-3 py-2 dark:border-surface-700">
    <span class="text-xs font-medium text-surface-500">Timeline</span>
    <span class="ml-2 text-xs text-surface-400">{entries.length}</span>
  </div>

  <div
    bind:this={container}
    class="min-h-0 flex-1 overflow-y-auto"
    role="listbox"
    aria-label="Action timeline"
  >
    {#if entries.length === 0}
      <p class="px-3 py-6 text-center text-xs text-surface-400">No events yet</p>
    {:else}
      {#each entries as entry (entry.index)}
        <button
          type="button"
          role="option"
          aria-selected={activeIndex === entry.index}
          data-timeline-index={entry.index}
          class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs transition-colors hover:bg-surface-100 dark:hover:bg-surface-800
            {activeIndex === entry.index
              ? 'bg-primary-50 dark:bg-primary-900/30'
              : ''}"
          onclick={() => onselect(entry.index)}
          onkeydown={(e) => onkeydown(e, entry.index)}
        >
          <span class="shrink-0 font-mono text-[10px] tabular-nums text-surface-400">
            {formatTime(entry.timestamp)}
          </span>
          <span class="mt-0.5 h-2 w-2 shrink-0 rounded-full {dotColor(entry.type)}"></span>
          <span class="min-w-0 flex-1 truncate text-surface-700 dark:text-surface-300">
            {entry.summary}
          </span>
        </button>
      {/each}
    {/if}
  </div>
</div>
