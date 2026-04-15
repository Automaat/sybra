<script lang="ts">
  import { LayoutDashboard } from '@lucide/svelte'
  import { workflowStore } from '../stores/workflows.svelte.js'

  interface Props {
    onselect: (id: string) => void
  }

  const { onselect }: Props = $props()

  $effect(() => {
    workflowStore.load()
    return () => {}
  })
</script>

<div class="flex flex-col gap-3 p-4 md:gap-4 md:p-6">
  <div class="flex items-center justify-between">
    <h2 class="text-lg font-semibold">Workflows</h2>
  </div>

  {#if workflowStore.loading && workflowStore.list.length === 0}
    <p class="text-sm opacity-60">Loading workflows...</p>
  {:else if workflowStore.error}
    <p class="text-sm text-error-500">{workflowStore.error}</p>
  {:else if workflowStore.list.length === 0}
    <div class="flex flex-col items-center gap-3 py-16 text-center">
      <LayoutDashboard size={48} class="text-surface-400" />
      <p class="text-sm text-surface-500">No workflows found</p>
    </div>
  {:else}
    <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {#each workflowStore.list as wf (wf.id)}
        <button
          type="button"
          class="flex flex-col gap-2 rounded-lg border border-surface-300 bg-surface-50 p-4 text-left transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
          onclick={() => onselect(wf.id)}
        >
          <div class="flex items-center gap-2">
            <LayoutDashboard size={20} class="shrink-0 text-surface-400" />
            <span class="text-sm font-semibold">{wf.name}</span>
            {#if wf.builtin}
              <span class="rounded px-1.5 py-0.5 text-xs font-medium bg-primary-100 text-primary-700 dark:bg-primary-900/40 dark:text-primary-300">built-in</span>
            {/if}
          </div>
          {#if wf.description}
            <p class="text-xs text-surface-500 dark:text-surface-400">{wf.description}</p>
          {/if}
          <div class="flex items-center gap-2 text-xs text-surface-400">
            <span>{wf.steps?.length ?? 0} steps</span>
            <span>
              trigger: {wf.trigger?.on ?? 'none'}{#if wf.trigger?.conditions?.length}
                ({wf.trigger.conditions.length} cond)
              {/if}
            </span>
          </div>
        </button>
      {/each}
    </div>
  {/if}
</div>
