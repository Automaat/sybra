<script lang="ts">
  import type { agent } from '../../../wailsjs/go/models.js'
  import { getAgentPhase, PHASE_CONFIG } from '$lib/agent-phases.js'
  import { taskStore } from '../../stores/tasks.svelte.js'
  import { agentStore } from '../../stores/agents.svelte.js'

  interface Props {
    agents: agent.Agent[]
    activeId: string
    onnavigate: (id: string) => void
  }

  const { agents, activeId, onnavigate }: Props = $props()

  const ACTIVE_PHASES = new Set(['running', 'blocked', 'waiting', 'human-required'])

  const activeAgents = $derived(
    agents.filter((a) => {
      const task = a.taskId ? taskStore.tasks.get(a.taskId) : null
      const phase = getAgentPhase(a.state, a.escalationReason, task?.status, a.awaitingApproval)
      return ACTIVE_PHASES.has(phase)
    }),
  )
</script>

<div class="flex flex-col overflow-hidden rounded-lg border border-surface-300 dark:border-surface-700">
  <div class="border-b border-surface-200 bg-surface-100 px-2.5 py-1.5 text-[10px] font-semibold uppercase tracking-wide text-surface-400 dark:border-surface-700 dark:bg-surface-800">
    Active agents
  </div>

  {#if activeAgents.length === 0}
    <div class="px-2.5 py-3 text-[11px] text-surface-400">No active agents</div>
  {:else}
    {#each activeAgents as a (a.id)}
      {@const linkedTask = a.taskId ? taskStore.tasks.get(a.taskId) : null}
      {@const phase = getAgentPhase(a.state, a.escalationReason, linkedTask?.status, a.awaitingApproval)}
      {@const config = PHASE_CONFIG[phase]}
      <button
        type="button"
        onclick={() => { onnavigate(a.id) }}
        class="flex w-full items-center gap-2 px-2.5 py-2 text-left text-[11px] transition-colors
          {a.id === activeId
            ? 'bg-primary-50 dark:bg-primary-950/40'
            : 'hover:bg-surface-100 dark:hover:bg-surface-800'}"
      >
        <span class="h-2 w-2 shrink-0 rounded-full {config.dotClasses} {config.animate ? 'animate-pulse-subtle' : ''}"></span>
        <span class="min-w-0 flex-1 truncate leading-snug" title={linkedTask?.title ?? a.name ?? a.id}>
          {linkedTask?.title ?? a.name ?? a.id}
        </span>
        {#if agentStore.stepTexts.get(a.id)}
          <span class="truncate text-[9px] italic text-surface-400 max-w-[80px]" title={agentStore.stepTexts.get(a.id)}>
            {agentStore.stepTexts.get(a.id)}
          </span>
        {/if}
      </button>
    {/each}
  {/if}
</div>
