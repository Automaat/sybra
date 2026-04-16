<script lang="ts">
  import { PRIORITY_OPTIONS } from '../lib/priorities.js'

  interface Props {
    currentPriority: string
    onpick: (priority: string) => void
    onclose: () => void
  }

  const { currentPriority, onpick, onclose }: Props = $props()

  let selectedIdx = $state(PRIORITY_OPTIONS.findIndex(p => p.value === (currentPriority ?? '')) || 0)

  $effect(() => {
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') { e.preventDefault(); e.stopImmediatePropagation(); onclose(); return }
      if (e.key === 'ArrowDown' || e.key === 'j') {
        e.preventDefault()
        selectedIdx = Math.min(selectedIdx + 1, PRIORITY_OPTIONS.length - 1)
        return
      }
      if (e.key === 'ArrowUp' || e.key === 'k') {
        e.preventDefault()
        selectedIdx = Math.max(selectedIdx - 1, 0)
        return
      }
      if (e.key === 'Enter') {
        e.preventDefault()
        onpick(PRIORITY_OPTIONS[selectedIdx].value)
        return
      }
    }
    window.addEventListener('keydown', handleKeydown, { capture: true })
    return () => window.removeEventListener('keydown', handleKeydown, { capture: true })
  })
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
  onclick={onclose}
  onkeydown={(e) => e.key === 'Escape' && onclose()}
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="min-w-[180px] rounded-xl border border-surface-300 bg-surface-50 shadow-2xl dark:border-surface-700 dark:bg-surface-900"
    onclick={(e) => e.stopPropagation()}
    onkeydown={(e) => e.stopPropagation()}
  >
    <div class="border-b border-surface-200 px-3 py-2 dark:border-surface-700">
      <p class="text-xs font-semibold uppercase tracking-wider text-surface-400">Priority <kbd class="ml-1 rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">P</kbd></p>
    </div>
    <ul class="py-1">
      {#each PRIORITY_OPTIONS as opt, i}
        <li>
          <button
            type="button"
            class="flex w-full items-center gap-3 px-3 py-1.5 text-left text-sm transition-colors {i === selectedIdx ? 'bg-primary-500/15 text-primary-700 dark:text-primary-300' : 'hover:bg-surface-200 dark:hover:bg-surface-700'}"
            onclick={() => onpick(opt.value)}
          >
            <span class="w-4 text-center font-mono {opt.classes}">{opt.icon}</span>
            <span>{opt.label}</span>
            {#if opt.value === (currentPriority ?? '')}
              <span class="ml-auto text-xs text-surface-400">current</span>
            {/if}
          </button>
        </li>
      {/each}
    </ul>
  </div>
</div>
