<script lang="ts">
  import { Handle, Position } from '@xyflow/svelte'
</script>

<Handle type="target" position={Position.Top} />

<div class="flex h-10 w-10 items-center justify-center rounded-full border-2 border-surface-400 bg-surface-200 dark:border-surface-500 dark:bg-surface-700">
  <svg class="h-4 w-4 text-surface-500 dark:text-surface-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 10a1 1 0 011-1h4a1 1 0 011 1v4a1 1 0 01-1 1h-4a1 1 0 01-1-1v-4z" />
  </svg>
</div>
