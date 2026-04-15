<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { EventsOn, RespondEscalation } from '$lib/api'
  import { agentStore } from '../stores/agents.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentState, agentEscalation, agentError } from '../lib/events.js'
  import StreamOutput from '../components/StreamOutput.svelte'
  import ChatView from '../components/ChatView.svelte'
  import AgentErrorBanner from '../components/AgentErrorBanner.svelte'
  import { getAgentPhase, PHASE_CONFIG } from '$lib/agent-phases.js'

  interface EscalationEvent {
    reason: string
    turnCount?: number
    costUsd?: number
    limit: number
  }

  interface AgentErrorEvent {
    kind: string
    msg: string
  }

  interface Props {
    agentId: string
    onback: () => void
    onviewtask: (taskId: string) => void
  }

  const { agentId, onback, onviewtask }: Props = $props()

  let a = $state<agent.Agent | null>(null)
  let error = $state('')
  let agentErr = $state<AgentErrorEvent | null>(null)
  let escalation = $state<EscalationEvent | null>(null)
  let escalationResponding = $state(false)
  let errorDismissed = $state(false)

  const isRunning = $derived(a?.state === 'running')

  // Seed error from cached agent state (already stopped with errorKind set).
  const cachedError = $derived(
    a?.errorKind ? { kind: a.errorKind, msg: a.errorMsg ?? '' } : null
  )
  const displayError = $derived(errorDismissed ? null : (agentErr ?? cachedError))

  const phase = $derived(
    a
      ? getAgentPhase(
          a.state,
          a.escalationReason,
          a.taskId ? taskStore.tasks.get(a.taskId)?.status : undefined,
        )
      : 'done',
  )
  const phaseConfig = $derived(PHASE_CONFIG[phase])

  $effect(() => {
    const cached = agentStore.agents.get(agentId)
    if (cached) a = cached

    const unsubState = EventsOn(agentState(agentId), (data: agent.Agent) => {
      a = data
      agentStore.updateAgent(agentId, data)
    })

    const unsubError = EventsOn(agentError(agentId), (data: AgentErrorEvent) => {
      agentErr = data
      errorDismissed = false
    })

    const unsubEscalation = EventsOn(agentEscalation(agentId), (data: EscalationEvent) => {
      escalation = data
      escalationResponding = false
    })

    return () => {
      unsubState()
      unsubError()
      unsubEscalation()
    }
  })

  async function handleStop() {
    try {
      await agentStore.stop(agentId)
    } catch (e) {
      error = String(e)
    }
  }

  async function handleEscalationContinue() {
    escalationResponding = true
    try {
      await RespondEscalation(agentId, true)
      escalation = null
    } catch (e) {
      error = String(e)
      escalationResponding = false
    }
  }

  async function handleEscalationKill() {
    escalationResponding = true
    try {
      await RespondEscalation(agentId, false)
      escalation = null
    } catch (e) {
      // For cost escalation the agent is already stopped — dismiss the banner.
      escalation = null
      escalationResponding = false
    }
  }

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleString()
  }
</script>

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
    </svg>
    Back to agents
  </button>

  {#if error}
    <p class="text-sm text-error-500">{error}</p>
  {/if}

  {#if displayError}
    <AgentErrorBanner
      {agentId}
      error={displayError}
      onretry={a?.taskId ? () => onviewtask(a!.taskId) : undefined}
      ondismiss={() => { agentErr = null; errorDismissed = true }}
    />
  {/if}

  {#if escalation}
    <div class="rounded-lg border-2 border-error-400 bg-error-50 p-4 dark:border-error-600 dark:bg-error-950">
      <div class="mb-3 flex items-center gap-2">
        <span class="rounded bg-error-200 px-2 py-0.5 text-xs font-bold text-error-800 dark:bg-error-700 dark:text-error-200">
          GUARDRAIL
        </span>
        {#if escalation.reason === 'turns'}
          <span class="text-sm font-medium text-surface-800 dark:text-surface-200">
            Turn limit reached — {escalation.turnCount} turns (limit: {escalation.limit})
          </span>
        {:else}
          <span class="text-sm font-medium text-surface-800 dark:text-surface-200">
            Cost limit exceeded — ${escalation.costUsd?.toFixed(2)} (limit: ${escalation.limit.toFixed(2)})
          </span>
        {/if}
      </div>
      <div class="flex items-center gap-2">
        {#if escalation.reason === 'turns'}
          <button
            type="button"
            disabled={escalationResponding}
            class="flex items-center gap-1.5 rounded-lg bg-success-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-success-700 disabled:opacity-50"
            onclick={handleEscalationContinue}
          >
            Continue
          </button>
        {/if}
        <button
          type="button"
          disabled={escalationResponding}
          class="flex items-center gap-1.5 rounded-lg bg-error-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-700 disabled:opacity-50"
          onclick={handleEscalationKill}
        >
          {escalation.reason === 'turns' ? 'Kill' : 'Dismiss'}
        </button>
      </div>
    </div>
  {/if}

  {#if a}
    <div class="flex flex-col gap-6">
      <div class="flex items-start justify-between gap-4">
        <div>
          <h1 class="text-2xl font-bold">{a.project || a.id}</h1>
          {#if a.name}
            <span class="text-sm text-surface-400">{a.name}</span>
          {/if}
          {#if phase === 'running'}
            <p class="mt-0.5 text-sm italic text-surface-400">
              {agentStore.stepTexts.get(agentId) ?? 'Working...'}
            </p>
          {:else if phase === 'blocked'}
            <p class="mt-0.5 text-sm text-tertiary-600 dark:text-tertiary-400">
              Awaiting tool approval
            </p>
          {:else if phase === 'human-required'}
            <p class="mt-0.5 text-sm font-medium text-error-600 dark:text-error-400">
              Waiting for human input
            </p>
          {:else if phase === 'reviewing'}
            <p class="mt-0.5 text-sm text-warning-600 dark:text-warning-400">
              Under review
            </p>
          {/if}
        </div>
        <div class="flex items-center gap-2">
          <span class="inline-flex items-center gap-1 rounded-full px-3 py-1 text-sm font-medium {phaseConfig.badgeClasses}">
            {#if phaseConfig.animate}
              <span class="h-2 w-2 animate-pulse-subtle rounded-full {phaseConfig.dotClasses}"></span>
            {:else if phase !== 'queued' && phase !== 'done'}
              <span class="h-2 w-2 rounded-full {phaseConfig.dotClasses}"></span>
            {/if}
            {phaseConfig.label}
          </span>
          {#if isRunning}
            <button
              type="button"
              class="rounded-lg bg-error-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-600"
              onclick={handleStop}
            >
              Stop
            </button>
          {/if}
        </div>
      </div>

      <div class="flex flex-wrap gap-6 text-sm">
        {#if a.taskId}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Task</span>
            <button
              type="button"
              class="text-left text-primary-500 hover:underline"
              onclick={() => onviewtask(a!.taskId)}
            >
              {a.taskId}
            </button>
          </div>
        {/if}
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Mode</span>
          <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{a.mode}</span>
        </div>
        {#if a.project}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Project</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{a.project}</span>
          </div>
        {/if}
        {#if a.name}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Session Name</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{a.name}</span>
          </div>
        {/if}
        {#if a.external}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Source</span>
            <span class="rounded bg-warning-200 px-2 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">external</span>
          </div>
        {/if}
        {#if a.pid}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">PID</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{a.pid}</span>
          </div>
        {/if}
        {#if a.command}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Command</span>
            <span class="max-w-md truncate rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{a.command}</span>
          </div>
        {/if}
        {#if a.sessionId}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Session</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{a.sessionId}</span>
          </div>
        {/if}
        {#if a.costUsd > 0}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Cost</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">${a.costUsd.toFixed(2)}</span>
          </div>
        {/if}
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Started</span>
          <span>{formatDate(a.startedAt)}</span>
        </div>
      </div>

      <div class="flex min-h-0 flex-1 flex-col gap-2">
        <span class="text-sm font-medium text-surface-500">Output</span>
        {#if a.mode === 'interactive'}
          <ChatView agentId={agentId} agentState={a.state} costUsd={a.costUsd} inputTokens={a.inputTokens ?? 0} outputTokens={a.outputTokens ?? 0} />
        {:else}
          <StreamOutput agentId={agentId} />
        {/if}
      </div>
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
