<script lang="ts">
  import { Tag, Check } from '@lucide/svelte'

  interface Props {
    tags: string[]
    selected: string[]
    onchange: (selected: string[]) => void
  }

  const { tags, selected, onchange }: Props = $props()

  let open = $state(false)
  let query = $state('')

  const filtered = $derived(
    query.trim() ? tags.filter((t) => t.toLowerCase().includes(query.trim().toLowerCase())) : tags,
  )

  function toggle(tag: string) {
    onchange(selected.includes(tag) ? selected.filter((t) => t !== tag) : [...selected, tag])
  }

  function close() {
    open = false
    query = ''
  }

  $effect(() => {
    if (!open) return
    function onKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        close()
      }
    }
    window.addEventListener('keydown', onKeydown, { capture: true })
    return () => window.removeEventListener('keydown', onKeydown, { capture: true })
  })
</script>

<div class="relative">
  <button
    type="button"
    class="flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs font-medium transition-colors {selected.length > 0
      ? 'border-primary-500 bg-primary-500/10 text-primary-700 dark:text-primary-300'
      : 'border-surface-300 bg-surface-50 hover:bg-surface-200 dark:border-surface-700 dark:bg-surface-800'}"
    onclick={() => (open = !open)}
  >
    <Tag size={14} />
    Filter by tag
    {#if selected.length > 0}
      <span class="rounded-full bg-primary-500 px-1.5 text-[10px] font-bold text-white">{selected.length}</span>
    {/if}
  </button>

  {#if open}
    <!-- Outside-click catcher -->
    <button type="button" class="fixed inset-0 z-40 cursor-default" aria-label="Close tag filter" onclick={close}></button>

    <div class="absolute left-0 z-50 mt-1 w-64 rounded-lg elevation-popover">
      <div class="border-b border-surface-200 p-2 dark:border-surface-700">
        <!-- svelte-ignore a11y_autofocus -->
        <input
          bind:value={query}
          type="text"
          placeholder="Search tags…"
          autofocus
          class="w-full rounded border border-surface-300 bg-surface-50 px-2 py-1 text-xs focus:border-primary-500 focus:outline-none dark:border-surface-700 dark:bg-surface-900"
        />
      </div>
      <ul class="max-h-64 overflow-y-auto py-1">
        {#each filtered as tag (tag)}
          <li>
            <button
              type="button"
              class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs transition-colors hover:bg-surface-200 dark:hover:bg-surface-700"
              onclick={() => toggle(tag)}
            >
              <span class="flex h-3.5 w-3.5 shrink-0 items-center justify-center rounded border {selected.includes(tag)
                ? 'border-primary-500 bg-primary-500 text-white'
                : 'border-surface-400'}">
                {#if selected.includes(tag)}<Check size={10} strokeWidth={3} />{/if}
              </span>
              <span class="truncate">{tag}</span>
            </button>
          </li>
        {:else}
          <li class="px-3 py-2 text-xs text-surface-400">No matching tags</li>
        {/each}
      </ul>
      {#if selected.length > 0}
        <div class="border-t border-surface-200 p-2 dark:border-surface-700">
          <button
            type="button"
            class="w-full rounded px-2 py-1 text-xs text-surface-500 hover:bg-surface-200 dark:hover:bg-surface-700"
            onclick={() => onchange([])}
          >
            Clear {selected.length} tag{selected.length === 1 ? '' : 's'}
          </button>
        </div>
      {/if}
    </div>
  {/if}
</div>
