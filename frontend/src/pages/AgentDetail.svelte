<script lang="ts">
  import { ChevronLeft } from '@lucide/svelte'
  import type { agent } from '../../wailsjs/go/models.js'
  import { EventsOn, RespondEscalation } from '$lib/api'
  import { agentStore } from '../stores/agents.svelte.js'
  import { convoStore } from '../stores/convo.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentState, agentEscalation, agentError } from '../lib/events.js'
  import AgentErrorBanner from '../components/AgentErrorBanner.svelte'
  import { getAgentPhase, PHASE_CONFIG } from '$lib/agent-phases.js'
  import { buildStreamTimeline, buildConvoTimeline } from '$lib/timeline.js'
  import { extractLatestPlanSteps, extractLatestPlanStepsFromConvo } from '$lib/plan-steps.js'
  import AgentHeader from '../components/agent-view/AgentHeader.svelte'
  import AgentViewBody from '../components/agent-view/AgentViewBody.svelte'

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
  let selectedIndex = $state<number | null>(null)

  // Seed error from cached agent state (already stopped with errorKind set).
  const cachedError = $derived(
    a?.errorKind ? { kind: a.errorKind, msg: a.errorMsg ?? '' } : null
  )
  const displayError = $derived(errorDismissed ? null : (agentErr ?? cachedError))

  const linkedTask = $derived(a?.taskId ? taskStore.tasks.get(a.taskId) : null)

  const phase = $derived(
    a
      ? getAgentPhase(
          a.state,
          a.escalationReason,
          linkedTask?.status,
          a.awaitingApproval,
        )
      : 'done',
  )
  const phaseConfig = $derived(PHASE_CONFIG[phase])

  // Timeline entries — reactive to store changes
  const streamOutputs = $derived(agentStore.outputs.get(agentId) ?? [])
  const convoEvents = $derived(convoStore.conversations.get(agentId) ?? [])
  const timelineEntries = $derived(
    a?.mode === 'interactive'
      ? buildConvoTimeline(convoEvents)
      : buildStreamTimeline(streamOutputs),
  )

  const planSteps = $derived(
    a?.mode === 'interactive'
      ? extractLatestPlanStepsFromConvo(convoEvents)
      : extractLatestPlanSteps(streamOutputs),
  )

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

  function onkeydown(e: KeyboardEvent) {
    // Don't hijack when typing in an input/textarea
    const tag = (e.target as HTMLElement).tagName
    if (tag === 'INPUT' || tag === 'TEXTAREA' || (e.target as HTMLElement).isContentEditable) return

    if (e.key === '[' || e.key === 'ArrowLeft') {
      e.preventDefault()
      selectedIndex = selectedIndex === null
        ? Math.max(0, timelineEntries.length - 1)
        : Math.max(0, selectedIndex - 1)
    } else if (e.key === ']' || e.key === 'ArrowRight') {
      e.preventDefault()
      selectedIndex = selectedIndex === null
        ? 0
        : Math.min(timelineEntries.length - 1, selectedIndex + 1)
    } else if (e.key === 'Escape') {
      selectedIndex = null
    }
  }
</script>

<svelte:window onkeydown={onkeydown} />

<div class="flex flex-col gap-4 p-4 md:gap-6 md:p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <ChevronLeft size={16} />
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
      <AgentHeader
        {a}
        {phase}
        {phaseConfig}
        stepText={agentStore.stepTexts.get(agentId)}
        {linkedTask}
        onstop={handleStop}
        {onviewtask}
      />

      <AgentViewBody
        {phase}
        {a}
        {linkedTask}
        {streamOutputs}
        {convoEvents}
        {timelineEntries}
        {planSteps}
        {selectedIndex}
        onselect={(i) => { selectedIndex = i }}
      />
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
