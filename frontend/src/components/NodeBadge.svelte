<script lang="ts">
  import Pill from './Pill.svelte'
  import { clusterStore } from '../stores/cluster.svelte.js'

  interface Props {
    node: string | undefined
  }

  const { node }: Props = $props()

  const status = $derived(clusterStore.statusOf(node))

  const statusClass = $derived(
    status === 'online'
      ? 'text-success-600 dark:text-success-400'
      : status === 'degraded'
        ? 'text-warning-600 dark:text-warning-400'
        : status === 'offline'
          ? 'text-error-600 dark:text-error-400'
          : 'text-surface-500',
  )
</script>

{#if node}
  <Pill role="reference" class={statusClass} title={`node ${node}: ${status || 'unknown'}`} data-testid="node-badge">
    ⬡ {node}
  </Pill>
{/if}
