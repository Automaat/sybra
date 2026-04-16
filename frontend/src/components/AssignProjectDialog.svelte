<script lang="ts">
  import { projectStore } from '../stores/projects.svelte.js'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
    onassign: (projectId: string) => void
  }

  const { open, onOpenChange, onassign }: Props = $props()

  let selectedIdx = $state(0)

  const projects = $derived(projectStore.list)

  $effect(() => {
    if (!open) return
    selectedIdx = 0
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') { e.preventDefault(); e.stopImmediatePropagation(); onOpenChange(false); return }
      if (e.key === 'ArrowDown' || e.key === 'j') {
        e.preventDefault()
        selectedIdx = Math.min(selectedIdx + 1, projects.length)
        return
      }
      if (e.key === 'ArrowUp' || e.key === 'k') {
        e.preventDefault()
        selectedIdx = Math.max(selectedIdx - 1, 0)
        return
      }
      if (e.key === 'Enter') {
        e.preventDefault()
        if (selectedIdx === 0) { onassign(''); onOpenChange(false) }
        else { onassign(projects[selectedIdx - 1]?.id ?? ''); onOpenChange(false) }
        return
      }
    }
    window.addEventListener('keydown', handleKeydown, { capture: true })
    return () => window.removeEventListener('keydown', handleKeydown, { capture: true })
  })
</script>

{#if open}
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
  onclick={() => onOpenChange(false)}
  onkeydown={(e) => e.key === 'Escape' && onOpenChange(false)}
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="min-w-[260px] rounded-xl border border-surface-300 bg-surface-50 shadow-2xl dark:border-surface-700 dark:bg-surface-900"
    onclick={(e) => e.stopPropagation()}
    onkeydown={(e) => e.stopPropagation()}
  >
    <div class="border-b border-surface-200 px-3 py-2 dark:border-surface-700">
      <p class="text-xs font-semibold uppercase tracking-wider text-surface-400">
        Add to Project <kbd class="ml-1 rounded bg-surface-200 px-1 py-0.5 font-mono text-xs dark:bg-surface-700">⇧C</kbd>
      </p>
    </div>
    <ul class="py-1">
      <li>
        <button
          type="button"
          class="flex w-full items-center px-3 py-1.5 text-left text-sm transition-colors {selectedIdx === 0 ? 'bg-primary-500/15 text-primary-700 dark:text-primary-300' : 'hover:bg-surface-200 dark:hover:bg-surface-700'}"
          onclick={() => { onassign(''); onOpenChange(false) }}
        >
          <span class="text-surface-400">No project</span>
        </button>
      </li>
      {#each projects as p, i}
        <li>
          <button
            type="button"
            class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm transition-colors {selectedIdx === i + 1 ? 'bg-primary-500/15 text-primary-700 dark:text-primary-300' : 'hover:bg-surface-200 dark:hover:bg-surface-700'}"
            onclick={() => { onassign(p.id); onOpenChange(false) }}
          >
            <span class="font-mono text-xs">{p.owner}/{p.repo}</span>
          </button>
        </li>
      {/each}
      {#if projects.length === 0}
        <li class="px-3 py-2 text-sm text-surface-400 italic">No projects available</li>
      {/if}
    </ul>
  </div>
</div>
{/if}
