<script lang="ts">
  import { SHORTCUTS } from '../lib/keyboard.svelte.js'

  interface Props {
    open: boolean
    onclose: () => void
  }

  const { open, onclose }: Props = $props()

  $effect(() => {
    if (!open) return
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        onclose()
      }
    }
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })
</script>

{#if open}
  <div class="fixed inset-0 z-50 flex items-center justify-center bg-black/40" role="presentation" onclick={onclose}>
    <div
      class="w-full max-w-xl rounded-xl border border-surface-300 bg-surface-50 shadow-2xl dark:border-surface-600 dark:bg-surface-800"
      role="dialog"
      aria-modal="true"
      aria-label="Keyboard shortcuts"
      tabindex="-1"
      onclick={(e) => e.stopPropagation()}
      onkeydown={(e) => e.stopPropagation()}
    >
      <div class="flex items-center justify-between border-b border-surface-300 px-5 py-3 dark:border-surface-600">
        <h2 class="text-sm font-semibold">Keyboard Shortcuts</h2>
        <button
          type="button"
          class="rounded bg-surface-200 px-1.5 py-0.5 font-mono text-xs text-surface-500 hover:bg-surface-300 dark:bg-surface-700 dark:hover:bg-surface-600"
          onclick={onclose}
        >Esc</button>
      </div>

      <div class="grid grid-cols-2 gap-6 p-5">
        {#each SHORTCUTS as group}
          <div>
            <h3 class="mb-2 text-xs font-semibold uppercase tracking-wider text-surface-400">{group.label}</h3>
            <ul class="space-y-1.5">
              {#each group.shortcuts as s}
                <li class="flex items-center justify-between gap-4">
                  <span class="text-sm text-surface-600 dark:text-surface-300">{s.description}</span>
                  <kbd class="shrink-0 rounded bg-surface-200 px-2 py-0.5 font-mono text-xs text-surface-500 dark:bg-surface-700 dark:text-surface-400">{s.keys}</kbd>
                </li>
              {/each}
            </ul>
          </div>
        {/each}
      </div>
    </div>
  </div>
{/if}
