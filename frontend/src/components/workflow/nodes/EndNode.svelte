<script lang="ts">
  import { Handle, Position } from '@xyflow/svelte'
  import { StopCircle } from '@lucide/svelte'
</script>

<Handle type="target" position={Position.Top} />

<div class="flex h-10 w-10 items-center justify-center rounded-full border-2 border-surface-400 bg-surface-200 dark:border-surface-500 dark:bg-surface-700">
  <StopCircle size={16} class="text-surface-500 dark:text-surface-400" />
</div>
