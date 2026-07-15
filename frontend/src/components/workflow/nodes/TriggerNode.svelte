<script lang="ts">
  import { Handle, Position } from '@xyflow/svelte'
  import { Zap } from '@lucide/svelte'
  import type { TriggerNodeData } from '../../../lib/workflow-graph.js'

  interface Props {
    data: TriggerNodeData
    selected?: boolean
  }

  const { data, selected = false }: Props = $props()

  const eventLabel = $derived(data.trigger?.on || '(no trigger)')
  const conditionCount = $derived(data.trigger?.conditions?.length ?? 0)
</script>

<div
  class="rounded-full border-2 border-amber-500 bg-amber-50 px-5 py-2 shadow-md transition-shadow dark:bg-amber-950/40"
  class:shadow-lg={selected}
  style="min-width: 180px;"
>
  <div class="flex items-center gap-2">
    <Zap size={16} class="text-amber-600 dark:text-amber-400" />
    <span class="text-xs font-semibold uppercase tracking-wide text-amber-700 dark:text-amber-300">
      Trigger
    </span>
  </div>
  <div class="mt-1 text-sm font-semibold text-surface-900 dark:text-surface-100">
    {eventLabel}
  </div>
  {#if conditionCount > 0}
    <div class="mt-0.5 text-xs text-surface-500 dark:text-surface-400">
      {conditionCount} condition{conditionCount === 1 ? '' : 's'}
    </div>
  {/if}
</div>

<Handle type="source" position={Position.Bottom} />
