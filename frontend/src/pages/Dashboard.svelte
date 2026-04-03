<script lang="ts">
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { reviewStore } from '../stores/reviews.svelte.js'
  import { MarkPRReady } from '../../wailsjs/go/main/App.js'
  import AgentCard from '../components/AgentCard.svelte'
  import PRCard from '../components/PRCard.svelte'

  interface Props {
    onviewagent: (agentId: string) => void
  }

  const { onviewagent }: Props = $props()

  const runningAgents = $derived(agentStore.byState('running'))
  const pausedAgents = $derived(agentStore.byState('paused'))
  const totalAgents = $derived(agentStore.list.length)

  const tasksByStatus = $derived({
    new: taskStore.byStatus('new').length,
    todo: taskStore.byStatus('todo').length,
    'in-progress': taskStore.byStatus('in-progress').length,
    'in-review': taskStore.byStatus('in-review').length,
    done: taskStore.byStatus('done').length,
  })
  const totalTasks = $derived(taskStore.list.length)

  const totalCost = $derived(
    agentStore.list.reduce((sum, a) => sum + (a.costUsd ?? 0), 0),
  )

  const draftPRs = $derived(reviewStore.createdByMe.filter((pr) => pr.isDraft))

  async function markReady(repo: string, number: number) {
    await MarkPRReady(repo, number)
    await reviewStore.load()
  }

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
      <span class="rounded bg-tertiary-200 px-2.5 py-1 text-xs text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
        New <strong>{tasksByStatus.new}</strong>
      </span>
      <span class="rounded bg-surface-200 px-2.5 py-1 text-xs dark:bg-surface-700">
        Todo <strong>{tasksByStatus.todo}</strong>
      </span>
      <span class="rounded bg-primary-200 px-2.5 py-1 text-xs text-primary-800 dark:bg-primary-700 dark:text-primary-200">
        In Progress <strong>{tasksByStatus['in-progress']}</strong>
      </span>
      <span class="rounded bg-success-200 px-2.5 py-1 text-xs text-success-800 dark:bg-success-700 dark:text-success-200">
        Done <strong>{tasksByStatus.done}</strong>
      </span>
      <span class="rounded bg-warning-200 px-2.5 py-1 text-xs text-warning-800 dark:bg-warning-700 dark:text-warning-200">
        In Review <strong>{tasksByStatus['in-review']}</strong>
      </span>
    </div>
  </div>

  <!-- Draft PRs -->
  {#if draftPRs.length > 0}
    <div class="flex flex-col gap-2">
      <span class="text-sm font-medium text-surface-500">Draft PRs</span>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {#each draftPRs as pr (pr.url)}
          <PRCard {pr} actionLabel="Ready for Review" onaction={() => markReady(pr.repository, pr.number)} />
        {/each}
      </div>
    </div>
  {/if}

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

</div>
