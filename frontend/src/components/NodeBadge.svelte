<script lang="ts">
  import Pill from './Pill.svelte'
  import { clusterStore } from '../stores/cluster.svelte.js'

  interface Props {
    node: string | undefined
  }

  const { node }: Props = $props()

  const status = $derived(clusterStore.statusOf(node))

  const dotClass = $derived(
    status === 'online'
      ? 'bg-success-500'
      : status === 'degraded'
        ? 'bg-warning-500'
        : status === 'offline'
          ? 'bg-error-500'
          : 'bg-surface-400',
  )
</script>

{#if node}
  <Pill role="reference" title={`node ${node}: ${status || 'unknown'}`} data-testid="node-badge">
    <span class="inline-block size-1.5 rounded-full {dotClass}" aria-hidden="true"></span>
    {node}
  </Pill>
{/if}
