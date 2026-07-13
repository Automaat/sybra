<script lang="ts">
  import { clusterStore } from '../stores/cluster.svelte.js'

  const dotClass = (status: string): string =>
    status === 'online'
      ? 'bg-success-500'
      : status === 'degraded'
        ? 'bg-warning-500'
        : status === 'offline'
          ? 'bg-error-500'
          : 'bg-surface-400'
</script>

{#if clusterStore.enabled}
  <div class="flex items-center gap-3 text-xs" data-testid="cluster-health">
    <span class="text-surface-500">Nodes</span>
    {#each clusterStore.nodes as node (node.name)}
      <span class="inline-flex items-center gap-1" title={node.lastError || node.status} data-testid={`cluster-node-${node.name}`}>
        <span class={`inline-block h-2 w-2 rounded-full ${dotClass(node.status)}`}></span>
        <span>{node.name}</span>
        <span class="text-surface-500">{node.status}</span>
      </span>
    {/each}
  </div>
{/if}
