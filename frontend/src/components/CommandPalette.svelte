<script lang="ts">
  import { Search, X } from '@lucide/svelte'
  import MobileSheet from './shell/MobileSheet.svelte'
  import { buildCommands, type Command, type PaletteCtx } from '../lib/palette-commands.js'
  import { score } from '../lib/fuzzy.js'
  import { getRecent, pushRecent } from '../lib/palette-recent.js'

  interface Props {
    open: boolean
    onclose: () => void
    ctx: PaletteCtx
  }

  const { open, onclose, ctx }: Props = $props()

  let query = $state('')
  let selectedIndex = $state(0)
  let inputRef = $state<HTMLInputElement | null>(null)

  const allCommands = $derived(buildCommands(ctx))

  const results = $derived.by((): Command[] => {
    const q = query.toLowerCase().trim()

    if (!q) {
      const recentIds = getRecent()
      const cmdMap = new Map(allCommands.map((c) => [c.id, c]))
      const recentCmds = recentIds.map((id) => cmdMap.get(id)).filter((c): c is Command => Boolean(c))
      const topActions = allCommands.filter((c) => c.section === 'action')
      const seen = new Set(recentCmds.map((c) => c.id))
      const extra = topActions.filter((c) => !seen.has(c.id))
      return [...recentCmds, ...extra].slice(0, 8)
    }

    const scored: { cmd: Command; total: number }[] = []
    for (const cmd of allCommands) {
      const ts = score(q, cmd.title)

      const ss = cmd.subtitle ? score(q, cmd.subtitle) : null

      let ks: number | null = null
      if (cmd.keywords?.length) {
        const best = Math.max(...cmd.keywords.map((kw) => score(q, kw) ?? -Infinity))
        if (isFinite(best)) ks = best
      }

      if (ts === null && ss === null && ks === null) continue

      const total = (ts ?? 0) + 0.5 * (ss ?? 0) + 0.3 * (ks ?? 0)
      scored.push({ cmd, total })
    }

    scored.sort((a, b) => b.total - a.total)
    return scored.slice(0, 15).map((s) => s.cmd)
  })

  $effect(() => {
    if (open) {
      query = ''
      selectedIndex = 0
      requestAnimationFrame(() => inputRef?.focus())
    }
  })

  $effect(() => {
    void query
    selectedIndex = 0
  })

  $effect(() => {
    if (!open) return
    function handleKeydown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopImmediatePropagation()
        onclose()
      } else if (e.key === 'ArrowDown') {
        e.preventDefault()
        selectedIndex = Math.min(selectedIndex + 1, results.length - 1)
      } else if (e.key === 'ArrowUp') {
        e.preventDefault()
        selectedIndex = Math.max(selectedIndex - 1, 0)
      } else if (e.key === 'Enter') {
        e.preventDefault()
        selectResult(results[selectedIndex])
      }
    }
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })

  function selectResult(cmd: Command | undefined) {
    if (!cmd) return
    pushRecent(cmd.id)
    cmd.run()
  }

  function sectionLabel(section: string): string {
    switch (section) {
      case 'action': return 'Actions'
      case 'page': return 'Pages'
      case 'task': return 'Tasks'
      case 'project': return 'Projects'
      case 'agent': return 'Agents'
      default: return section
    }
  }
</script>

<MobileSheet {open} onOpenChange={(o) => { if (!o) onclose() }} variant="top" backdropClass="fixed inset-0 z-40 bg-black/40 backdrop-blur-sm">
  <div class="flex flex-col">
    <!-- Search bar -->
    <div class="flex items-center gap-3 border-b border-surface-200 px-4 dark:border-surface-700">
      <Search size={16} class="shrink-0 transition-colors {query ? 'text-primary-500' : 'text-surface-400'}" />
      <input
        bind:this={inputRef}
        bind:value={query}
        type="text"
        placeholder="Search tasks, projects, agents, pages..."
        class="flex-1 bg-transparent py-3.5 text-base outline-none placeholder:text-surface-400 md:text-sm"
      />
      <button
        type="button"
        onclick={onclose}
        class="tap -mr-2 flex items-center gap-1.5 text-surface-400 hover:text-surface-600 dark:hover:text-surface-300"
        aria-label="Close"
      >
        <kbd class="hidden rounded border border-surface-300 px-1.5 py-0.5 font-mono text-[10px] text-surface-400 dark:border-surface-600 md:inline">Esc</kbd>
        <X size={20} class="md:hidden" />
      </button>
    </div>

    <!-- Results -->
    <ul class="max-h-[60dvh] overflow-y-auto py-1 md:max-h-80" role="listbox" aria-label="Command palette results">
      {#if results.length === 0}
        <li class="px-4 py-8 text-center text-sm text-surface-400">
          No results for "<span class="text-surface-700 dark:text-surface-300">{query}</span>"
        </li>
      {:else}
        {#each results as cmd, i}
          {#if i === 0 || results[i - 1].section !== cmd.section}
            <li role="presentation" class="px-3 pb-0.5 pt-2 text-[10px] font-semibold uppercase tracking-wider text-surface-400 first:pt-1.5">
              {sectionLabel(cmd.section)}
            </li>
          {/if}
          <li role="option" aria-selected={i === selectedIndex}>
            <button
              type="button"
              class="tap flex w-full items-center gap-3 border-l-2 px-3 py-2 text-left text-sm transition-colors
                {i === selectedIndex
                  ? 'border-primary-500 bg-primary-50 text-primary-900 dark:bg-primary-950/50 dark:text-primary-100'
                  : 'border-transparent hover:bg-surface-100 dark:hover:bg-surface-800'}"
              onclick={() => selectResult(cmd)}
              onmouseenter={() => { selectedIndex = i }}
            >
              <span class="flex-1 font-medium">{cmd.title}</span>
              {#if cmd.subtitle}
                <span class="shrink-0 text-xs text-surface-400">{cmd.subtitle}</span>
              {/if}
              {#if cmd.shortcut}
                <kbd class="shrink-0 rounded border border-surface-300 px-1.5 py-0.5 font-mono text-[10px] text-surface-400 dark:border-surface-600">{cmd.shortcut}</kbd>
              {:else if i === selectedIndex}
                <span class="shrink-0 font-mono text-xs text-primary-500">↵</span>
              {/if}
            </button>
          </li>
        {/each}
      {/if}
    </ul>

    <!-- Footer hints -->
    <div class="hidden items-center gap-4 border-t border-surface-200 px-4 py-2.5 dark:border-surface-700 md:flex">
      <div class="flex gap-4 text-xs text-surface-400">
        <span class="flex items-center gap-1">
          <kbd class="rounded border border-surface-300 px-1 py-0.5 font-mono text-[10px] dark:border-surface-600">↑↓</kbd>
          navigate
        </span>
        <span class="flex items-center gap-1">
          <kbd class="rounded border border-surface-300 px-1 py-0.5 font-mono text-[10px] dark:border-surface-600">↵</kbd>
          open
        </span>
        <span class="flex items-center gap-1">
          <kbd class="rounded border border-surface-300 px-1 py-0.5 font-mono text-[10px] dark:border-surface-600">Esc</kbd>
          close
        </span>
      </div>
    </div>
  </div>
</MobileSheet>
