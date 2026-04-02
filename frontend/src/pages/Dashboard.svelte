<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import AgentCard from '../components/AgentCard.svelte'

  interface Props {
    onviewagent: (agentId: string) => void
    onviewtask: (taskId: string) => void
  }

  const { onviewagent, onviewtask }: Props = $props()

  const runningAgents = $derived(agentStore.byState('running'))
  const pausedAgents = $derived(agentStore.byState('paused'))
  const totalAgents = $derived(agentStore.list.length)

  const tasksByStatus = $derived({
    todo: taskStore.byStatus('todo').length,
    'in-progress': taskStore.byStatus('in-progress').length,
    done: taskStore.byStatus('done').length,
    blocked: taskStore.byStatus('blocked').length,
  })
  const totalTasks = $derived(taskStore.list.length)

  const totalCost = $derived(
    agentStore.list.reduce((sum, a) => sum + (a.costUsd ?? 0), 0),
  )

  const recentTasks = $derived(taskStore.list.slice(0, 5))
</script>

<div class="flex flex-col gap-6 p-6">
  <h1 class="text-2xl font-bold">Dashboard</h1>

  <!-- Stats row -->
  <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5">
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <span class="text-xs font-medium text-surface-500">Running Agents</span>
      <p class="mt-1 text-2xl font-bold text-success-600 dark:text-success-400">{runningAgents.length}</p>
    </div>
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <span class="text-xs font-medium text-surface-500">Waiting for Input</span>
      <p class="mt-1 text-2xl font-bold text-warning-600 dark:text-warning-400">{pausedAgents.length}</p>
    </div>
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <span class="text-xs font-medium text-surface-500">Total Tasks</span>
      <p class="mt-1 text-2xl font-bold">{totalTasks}</p>
    </div>
    <div class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
      <span class="text-xs font-medium text-surface-500">Total Cost</span>
      <p class="mt-1 text-2xl font-bold">${totalCost.toFixed(2)}</p>
    </div>
  </div>

  <!-- Task status breakdown -->
  <div class="flex flex-col gap-2">
    <span class="text-sm font-medium text-surface-500">Task Status</span>
    <div class="flex gap-3">
      <span class="rounded bg-surface-200 px-2.5 py-1 text-xs dark:bg-surface-700">
        Todo <strong>{tasksByStatus.todo}</strong>
      </span>
      <span class="rounded bg-primary-200 px-2.5 py-1 text-xs text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        In Progress <strong>{tasksByStatus['in-progress']}</strong>
      </span>
      <span class="rounded bg-success-200 px-2.5 py-1 text-xs text-success-800 dark:bg-success-700 dark:text-success-200">
        Done <strong>{tasksByStatus.done}</strong>
      </span>
      <span class="rounded bg-error-200 px-2.5 py-1 text-xs text-error-800 dark:bg-error-700 dark:text-error-200">
        Blocked <strong>{tasksByStatus.blocked}</strong>
      </span>
    </div>
  </div>

  <!-- Running + waiting agents -->
  {#if runningAgents.length > 0 || pausedAgents.length > 0}
    <div class="flex flex-col gap-2">
      <span class="text-sm font-medium text-surface-500">Active Agents</span>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {#each [...runningAgents, ...pausedAgents] as a (a.id)}
          <AgentCard agent={a} onclick={() => onviewagent(a.id)} />
        {/each}
      </div>
    </div>
  {/if}

  <!-- Recent tasks -->
  {#if recentTasks.length > 0}
    <div class="flex flex-col gap-2">
      <span class="text-sm font-medium text-surface-500">Recent Tasks</span>
      <div class="flex flex-col gap-1">
        {#each recentTasks as t (t.id)}
          <button
            type="button"
            class="flex items-center justify-between rounded-lg border border-surface-300 bg-surface-50 px-4 py-2.5 text-left text-sm transition-colors hover:bg-surface-100 dark:border-surface-600 dark:bg-surface-800 dark:hover:bg-surface-700"
            onclick={() => onviewtask(t.id)}
          >
            <span class="font-medium">{t.title}</span>
            <span class="rounded px-2 py-0.5 text-xs
              {t.status === 'done' ? 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' :
               t.status === 'in-progress' ? 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' :
               t.status === 'blocked' ? 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200' :
               'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
              {t.status}
            </span>
          </button>
        {/each}
      </div>
    </div>
  {/if}
</div>
