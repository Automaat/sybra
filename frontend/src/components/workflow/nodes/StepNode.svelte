<script lang="ts">
  import { Handle, Position } from '@xyflow/svelte'
  import { stepTypeColors, type StepNodeData } from '../../../lib/workflow-graph.js'

  interface Props {
    data: StepNodeData
    selected?: boolean
  }

  const { data, selected = false }: Props = $props()

  const color = $derived(stepTypeColors[data.stepType] ?? '#6b7280')

  const typeLabel = $derived(
    data.stepType === 'run_agent' ? 'Agent' :
    data.stepType === 'wait_human' ? 'Human' :
    data.stepType === 'set_status' ? 'Status' :
    data.stepType === 'condition' ? 'Condition' :
    data.stepType === 'shell' ? 'Shell' :
    data.stepType === 'parallel' ? 'Parallel' :
    data.stepType
  )

  const roleLabel = $derived(
    data.step?.config?.role
      ? ` (${data.step.config.role})`
      : ''
  )
</script>

<Handle type="target" position={Position.Top} />

<div
  class="rounded-lg border-2 bg-white px-4 py-3 shadow-md transition-shadow dark:bg-surface-800"
  class:shadow-lg={selected}
  style="border-color: {color}; min-width: 160px;"
>
  <div class="flex items-center gap-2">
    <span
      class="inline-block h-2.5 w-2.5 rounded-full"
      style="background: {color};"
    ></span>
    <span class="text-sm font-semibold text-surface-900 dark:text-surface-100">
      {data.label}
    </span>
  </div>
  <div class="mt-1 text-xs text-surface-500 dark:text-surface-400">
    {typeLabel}{roleLabel}
  </div>
  {#if data.step?.config?.mode}
    <div class="mt-0.5 text-xs text-surface-400 dark:text-surface-500">
      {data.step.config.mode}
    </div>
  {/if}
</div>

<Handle type="source" position={Position.Bottom} />
