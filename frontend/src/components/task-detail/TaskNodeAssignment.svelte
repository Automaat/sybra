<script lang="ts">
  import { Server } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { clusterStore } from '../../stores/cluster.svelte.js'
  import { taskStore } from '../../stores/tasks.svelte.js'

  interface Props {
    task: Task
  }

  const { task }: Props = $props()

  const LOCAL = 'local'

  let busy = $state(false)
  let error = $state('')

  const current = $derived(task.assignedNode || LOCAL)
  const status = $derived(clusterStore.statusOf(task.assignedNode))
  const options = $derived([LOCAL, ...clusterStore.names])

  async function reassign(node: string) {
    if (node === current || busy) return
    busy = true
    error = ''
    try {
      await taskStore.reassign(task.id, node)
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
    } finally {
      busy = false
    }
  }
</script>

{#if clusterStore.enabled}
  <div class="flex items-center gap-1.5" data-testid="task-node-assignment">
    <Server size={13} class="shrink-0 text-surface-400" />
    <select
      class="bg-transparent font-mono text-surface-700 disabled:opacity-50 dark:text-surface-300"
      value={current}
      disabled={busy}
      aria-label="Execution node"
      title={task.assignedNode ? `node ${task.assignedNode}: ${status || 'unknown'}` : 'runs on the leader'}
      onchange={(e) => reassign((e.currentTarget as HTMLSelectElement).value)}
    >
      {#each options as node (node)}
        <option value={node}>{node}</option>
      {/each}
    </select>
    {#if error}
      <span class="text-xs text-error-500" data-testid="task-node-error">{error}</span>
    {/if}
  </div>
{/if}
