<script lang="ts">
  import { ChevronRight } from '@lucide/svelte'
  import MobileSheet from './MobileSheet.svelte'
  import { navStore, type Page } from '../../lib/navigation.svelte.js'
  import { taskStore } from '../../stores/tasks.svelte.js'

  interface Props {
    open: boolean
    onOpenChange: (open: boolean) => void
  }

  const { open, onOpenChange }: Props = $props()

  const reviewCount = $derived(
    taskStore.byStatus('plan-review').length + taskStore.byStatus('test-plan-review').length
  )

  type Item = { label: string; page: Page; badge?: number }

  const items: Item[] = $derived([
    { label: 'Dashboard', page: { kind: 'dashboard' } },
    { label: 'Projects', page: { kind: 'project-list' } },
    { label: 'GitHub', page: { kind: 'github' } },
    { label: 'Reviews', page: { kind: 'reviews' }, badge: reviewCount },
    { label: 'Workflows', page: { kind: 'workflows' } },
    { label: 'Stats', page: { kind: 'stats' } },
    { label: 'Settings', page: { kind: 'settings' } },
  ])

  function go(p: Page) {
    navStore.reset(p)
    onOpenChange(false)
  }
</script>

<MobileSheet {open} {onOpenChange} variant="bottom" title="More">
  <ul class="flex flex-col px-2 pb-3">
    {#each items as item (item.label)}
      <li>
        <button
          type="button"
          onclick={() => go(item.page)}
          class="tap flex w-full items-center justify-between gap-3 rounded-lg px-4 py-3 text-left text-base font-medium active:bg-surface-200 dark:active:bg-surface-800"
        >
          <span>{item.label}</span>
          <span class="flex items-center gap-2 text-surface-400">
            {#if item.badge}
              <span class="rounded-full bg-warning-500 px-2 py-0.5 text-xs font-bold text-white">{item.badge}</span>
            {/if}
            <ChevronRight size={16} />
          </span>
        </button>
      </li>
    {/each}
  </ul>
</MobileSheet>
