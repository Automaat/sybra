<script lang="ts">
  import { ChevronLeft } from '@lucide/svelte'
  import type { workflow } from '../../wailsjs/go/models.js'

  interface Props {
    def: workflow.Definition | null
    onback: () => void
  }

  const { def, onback }: Props = $props()
</script>

<div class="flex h-full min-h-0 flex-col">
  <div class="flex shrink-0 items-center gap-3 border-b border-surface-300 px-4 py-2 dark:border-surface-700">
    <button
      type="button"
      onclick={onback}
      class="tap -ml-2 rounded-lg active:bg-surface-200 dark:active:bg-surface-700"
      aria-label="Back"
    >
      <ChevronLeft size={24} />
    </button>
    <h2 class="truncate text-base font-semibold">{def?.name ?? 'Loading...'}</h2>
  </div>

  <div class="flex-1 overflow-y-auto p-4">
    <div class="mb-4 rounded-lg border border-warning-300 bg-warning-50 p-4 text-sm text-warning-800 dark:border-warning-700 dark:bg-warning-900/20 dark:text-warning-300">
      <p class="font-semibold">Read-only on mobile</p>
      <p class="mt-1 text-xs">Workflows use a graph editor that needs a larger screen and a pointer. Open Sybra on a desktop to edit.</p>
    </div>

    {#if def}
      <section class="mb-4 rounded-lg border border-surface-300 bg-surface-50 p-3 dark:border-surface-700 dark:bg-surface-900">
        <h3 class="mb-2 text-xs font-semibold uppercase tracking-wider text-surface-500">Trigger</h3>
        <p class="text-sm">
          <span class="font-mono">{def.trigger?.on || 'none'}</span>
        </p>
        {#if def.trigger?.conditions?.length}
          <ul class="mt-2 space-y-1 text-xs text-surface-500">
            {#each def.trigger.conditions as c}
              <li class="font-mono">• {c.field} {c.operator} {c.value}</li>
            {/each}
          </ul>
        {/if}
      </section>

      <section class="rounded-lg border border-surface-300 bg-surface-50 p-3 dark:border-surface-700 dark:bg-surface-900">
        <h3 class="mb-2 text-xs font-semibold uppercase tracking-wider text-surface-500">Steps ({def.steps?.length ?? 0})</h3>
        {#if (def.steps?.length ?? 0) === 0}
          <p class="text-sm italic text-surface-400">No steps</p>
        {:else}
          <ol class="space-y-2">
            {#each def.steps ?? [] as step, i (step.id)}
              <li class="rounded border border-surface-300 bg-surface-100 px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-800">
                <div class="flex items-center gap-2">
                  <span class="rounded bg-primary-500 px-1.5 py-0.5 text-xs font-bold text-white">{i + 1}</span>
                  <span class="font-medium">{step.name || step.id}</span>
                </div>
                {#if step.type}
                  <p class="mt-1 font-mono text-xs text-surface-500">{step.type}</p>
                {/if}
              </li>
            {/each}
          </ol>
        {/if}
      </section>
    {:else}
      <p class="text-sm opacity-60">Loading workflow...</p>
    {/if}
  </div>
</div>
