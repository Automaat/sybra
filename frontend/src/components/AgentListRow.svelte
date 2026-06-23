<script lang="ts">
  import type { Agent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { getAgentPhase, PHASE_CONFIG } from '$lib/agent-phases.js'
  import AgentStateBadge from './AgentStateBadge.svelte'
  import { clock } from '$lib/clock.svelte.js'
  import { formatElapsed } from '$lib/elapsed.js'
  import { timeAgo } from '$lib/dates.js'
  import { agentDisplayName, cleanAgentName } from '$lib/agent-name.js'

  interface Props {
    agent: Agent
    onclick: () => void
  }

  const { agent: a, onclick }: Props = $props()

  const linkedTask = $derived(a.taskId ? taskStore.tasks.get(a.taskId) : null)
  const heading = $derived(agentDisplayName(a, linkedTask?.title))
  const subtitle = $derived(cleanAgentName(a.name))

  const phase = $derived(getAgentPhase(a.state, a.escalationReason, linkedTask?.status, a.awaitingApproval))
  const config = $derived(PHASE_CONFIG[phase])

  const isActivePhase = $derived(
    phase === 'running' || phase === 'blocked' || phase === 'waiting' || phase === 'human-required',
  )

  // Single-line intent under the name: live step text while running, the
  // pending action otherwise, falling back to a distinct session name.
  const intent = $derived.by(() => {
    switch (phase) {
      case 'running':
        return agentStore.stepTexts.get(a.id) ?? 'Working...'
      case 'human-required':
        return 'Waiting for human input'
      case 'waiting':
        return 'Waiting for reply'
      case 'blocked':
        return 'Awaiting tool approval'
      default:
        return subtitle && subtitle !== heading ? subtitle : ''
    }
  })
</script>

<button
  type="button"
  class="flex w-full items-center gap-3 rounded-lg border border-surface-200 bg-white px-3 py-2 text-left transition-colors hover:bg-surface-50 dark:border-surface-700 dark:bg-surface-800 dark:hover:bg-surface-700 {config.faded ? 'opacity-60' : ''}"
  onclick={onclick}
>
  <div class="min-w-0 flex-1">
    <div class="flex items-center gap-2">
      <span class="truncate text-sm font-medium text-surface-900 dark:text-surface-100">{heading}</span>
      {#if a.external}
        <span class="shrink-0 rounded bg-warning-100 px-1.5 py-0.5 text-[10px] text-warning-700 dark:bg-warning-900 dark:text-warning-300">external</span>
      {/if}
      {#if a.project}
        <span class="shrink-0 truncate rounded bg-surface-100 px-1.5 py-0.5 text-[10px] text-surface-500 dark:bg-surface-700">{a.project}</span>
      {/if}
    </div>
    {#if intent}
      <p class="truncate text-xs {phase === 'human-required' ? 'font-medium text-error-600 dark:text-error-400' : 'text-surface-500'}">{intent}</p>
    {/if}
  </div>

  <AgentStateBadge {phase} />

  <div class="flex shrink-0 flex-col items-end gap-0.5">
    {#if a.costUsd > 0}
      <span class="text-xs text-surface-500">${a.costUsd.toFixed(2)}</span>
    {/if}
    <span class="text-[10px] text-surface-400">
      {isActivePhase ? formatElapsed(a.startedAt, clock.now) : timeAgo(a.startedAt)}
    </span>
  </div>
</button>
