<script lang="ts">
  import { statusOptionsFor, coreStatus } from '../lib/statuses.js'

  interface Props {
    currentStatus: string
    onpick: (status: string) => void
    onclose: () => void
  }

  const { currentStatus, onpick, onclose }: Props = $props()

  const options = $derived(statusOptionsFor(currentStatus))

  // Highlight the core status the current (possibly granular) status rolls up to.
  function initialIdx(): number {
    const opts = statusOptionsFor(currentStatus)
    return Math.max(0, opts.findIndex((s) => s.value === coreStatus(currentStatus)))
  }
  let selectedIdx = $state(initialIdx())

  // Picking the bucket the task is already in is a no-op — don't overwrite a
  // granular status (e.g. blocked) with its rolled-up core (human-required)
  // just because the user confirmed the highlighted "current" option.
  function pick(value: string) {
    if (value === coreStatus(currentStatus)) { onclose(); return }
    onpick(value)
  }

  $effect(() => {
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') { e.preventDefault(); e.stopImmediatePropagation(); onclose(); return }
      if (e.key === 'ArrowDown' || e.key === 'j') {
        e.preventDefault()
        selectedIdx = Math.min(selectedIdx + 1, options.length - 1)
        return
      }
      if (e.key === 'ArrowUp' || e.key === 'k') {
        e.preventDefault()
        selectedIdx = Math.max(selectedIdx - 1, 0)
        return
      }
      if (e.key === 'Enter') {
        e.preventDefault()
        pick(options[selectedIdx].value)
        return
      }
    }
    window.addEventListener('keydown', handleKeydown, { capture: true })
    return () => window.removeEventListener('keydown', handleKeydown, { capture: true })
  })
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="fixed inset-0 z-50 flex items-center justify-center modal-backdrop"
  onclick={onclose}
  onkeydown={(e) => e.key === 'Escape' && onclose()}
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="min-w-[200px] rounded-xl elevation-popover"
    onclick={(e) => e.stopPropagation()}
    onkeydown={(e) => e.stopPropagation()}
  >
    <div class="border-b border-surface-200 px-3 py-2 dark:border-surface-700">
      <p class="text-xs font-semibold uppercase tracking-wider text-surface-400">Change Status <kbd class="ml-1 rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">S</kbd></p>
    </div>
    <ul class="py-1">
      {#each options as opt, i}
        <li>
          <button
            type="button"
            class="flex w-full items-center justify-between gap-3 px-3 py-1.5 text-left text-sm transition-colors {i === selectedIdx ? 'bg-primary-500/15 text-primary-700 dark:text-primary-300' : 'hover:bg-surface-200 dark:hover:bg-surface-700'}"
            onclick={() => pick(opt.value)}
          >
            <span>{opt.label}</span>
            {#if opt.value === coreStatus(currentStatus)}
              <span class="text-xs text-surface-400">current</span>
            {/if}
          </button>
        </li>
      {/each}
    </ul>
  </div>
</div>
