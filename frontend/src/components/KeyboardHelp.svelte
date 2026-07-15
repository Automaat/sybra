<script lang="ts">
  import MobileSheet from './shell/MobileSheet.svelte'
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

<MobileSheet {open} onOpenChange={(o) => { if (!o) onclose() }} variant="center" title="Keyboard Shortcuts">
  <div class="grid grid-cols-1 gap-5 p-5 md:grid-cols-2 md:gap-6">
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
</MobileSheet>
