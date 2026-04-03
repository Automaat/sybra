<script lang="ts">
  import snarkdown from 'snarkdown'
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
    new: taskStore.byStatus('new').length,
    todo: taskStore.byStatus('todo').length,
    'in-progress': taskStore.byStatus('in-progress').length,
    'in-review': taskStore.byStatus('in-review').length,
    'human-required': taskStore.byStatus('human-required').length,
    done: taskStore.byStatus('done').length,
  })
  const totalTasks = $derived(taskStore.list.length)

  const totalCost = $derived(
    agentStore.list.reduce((sum, a) => sum + (a.costUsd ?? 0), 0),
  )

  const recentTasks = $derived(taskStore.list.slice(0, 5))
  const humanRequiredTasks = $derived(taskStore.byStatus('human-required'))

  let expandedTaskId = $state<string | null>(null)
  let promptText = $state('')
  let submitting = $state(false)
  let submitError = $state('')

  async function submitPrompt(taskId: string) {
    if (!promptText.trim()) return
    submitting = true
    submitError = ''
    try {
      await agentStore.start(taskId, 'headless', promptText.trim())
      await taskStore.update(taskId, { status: 'in-progress' })
      promptText = ''
      expandedTaskId = null
    } catch (e) {
      submitError = String(e)
    } finally {
      submitting = false
    }
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
      <span class="rounded bg-error-200 px-2.5 py-1 text-xs text-error-800 dark:bg-error-700 dark:text-error-200">
        Human Required <strong>{tasksByStatus['human-required']}</strong>
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

  <!-- Human required -->
  {#if humanRequiredTasks.length > 0}
    <div class="flex flex-col gap-2">
      <span class="text-sm font-medium text-error-500">Human Required ({humanRequiredTasks.length})</span>
      <div class="flex flex-col gap-1">
        {#each humanRequiredTasks as t (t.id)}
          <div class="rounded-lg border border-error-300 bg-error-50 dark:border-error-700 dark:bg-error-950">
            <button
              type="button"
              class="flex w-full items-center justify-between px-4 py-2.5 text-left text-sm transition-colors hover:bg-error-100 dark:hover:bg-error-900"
              onclick={() => { expandedTaskId = expandedTaskId === t.id ? null : t.id; promptText = ''; submitError = '' }}
            >
              <span class="font-medium">{t.title}</span>
              <svg class="h-4 w-4 text-error-500 transition-transform {expandedTaskId === t.id ? 'rotate-180' : ''}" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
              </svg>
            </button>
            {#if expandedTaskId === t.id}
              <div class="flex flex-col gap-2 border-t border-error-300 px-4 py-3 dark:border-error-700">
                {#if t.body}
                  <div class="prose prose-sm max-w-none text-surface-700 dark:text-surface-300 [&_h1]:text-base [&_h1]:font-semibold [&_h2]:text-sm [&_h2]:font-semibold [&_h3]:text-sm [&_h3]:font-medium [&_p]:text-xs [&_li]:text-xs [&_ul]:list-disc [&_ul]:pl-4 [&_ol]:list-decimal [&_ol]:pl-4 [&_strong]:font-semibold [&_code]:rounded [&_code]:bg-surface-200 [&_code]:px-1 [&_code]:text-xs [&_code]:dark:bg-surface-700 [&_a]:text-primary-500 [&_a]:underline">
                    {@html snarkdown(t.body)}
                  </div>
                {/if}
                {#if submitError}
                  <p class="text-xs text-error-500">{submitError}</p>
                {/if}
                <textarea
                  class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-2 text-sm dark:border-surface-600 dark:bg-surface-800"
                  rows="2"
                  placeholder="Tell the agent what to do..."
                  bind:value={promptText}
                  onkeydown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); submitPrompt(t.id) } }}
                ></textarea>
                <button
                  type="button"
                  class="w-fit rounded-lg bg-primary-500 px-3 py-1.5 text-xs font-medium text-white hover:bg-primary-600 disabled:opacity-50"
                  onclick={() => submitPrompt(t.id)}
                  disabled={submitting || !promptText.trim()}
                >
                  {submitting ? 'Starting...' : 'Send to agent'}
                </button>
              </div>
            {/if}
          </div>
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
               t.status === 'in-review' ? 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' :
               t.status === 'human-required' ? 'bg-error-200 text-error-800 dark:bg-error-700 dark:text-error-200' :
               t.status === 'new' ? 'bg-tertiary-200 text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200' :
               'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
              {t.status}
            </span>
          </button>
        {/each}
      </div>
    </div>
  {/if}
</div>
