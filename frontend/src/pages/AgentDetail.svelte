<script lang="ts">
  import { untrack } from 'svelte'
  import { ChevronLeft } from '@lucide/svelte'
  import type { Agent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import { EventsOn, RespondEscalation } from '$lib/api'
  import { getAgentForNode } from '$lib/api-cluster'
  import { agentStore } from '../stores/agents.svelte.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentState, agentEscalation, agentError, agentPluginErrors } from '../lib/events.js'
  import AgentErrorBanner from '../components/AgentErrorBanner.svelte'
  import { getAgentPhase } from '$lib/agent-phases.js'
  import { buildStreamTimeline } from '$lib/timeline.js'
  import { extractLatestPlanSteps } from '$lib/plan-steps.js'
  import AgentHeader from '../components/agent-view/AgentHeader.svelte'
  import AgentViewBody from '../components/agent-view/AgentViewBody.svelte'
  import type { ToolUseSignal } from '$lib/workspace-tabs.js'

  interface EscalationEvent {
    reason: string
    guardrail?: string
    measurement?: string
    costSource?: string
    turnCount?: number
    costUsd?: number
    measuredValue?: number
    limit: number
  }

  interface AgentErrorEvent {
    kind: string
    msg: string
  }

  interface PluginErrorsEvent {
    errors: string[]
  }

  interface Props {
    agentId: string
    onback: () => void
    onviewtask: (taskId: string) => void
    onnavigate?: (id: string) => void
  }

  const { agentId, onback, onviewtask, onnavigate }: Props = $props()

  let a = $state<Agent | null>(null)
  let error = $state('')
  let agentErr = $state<AgentErrorEvent | null>(null)
  let pluginErrors = $state<string[]>([])
  let pluginErrorsDismissed = $state(false)
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

  // Timeline entries — reactive to store changes
  const streamOutputs = $derived(agentStore.outputs.get(agentId) ?? [])
  const timelineEntries = $derived(buildStreamTimeline(streamOutputs))
  const planSteps = $derived(extractLatestPlanSteps(streamOutputs))
  const allAgents = $derived(agentStore.list)

  const latestToolUse = $derived.by<ToolUseSignal | undefined>(() => {
    for (let i = streamOutputs.length - 1; i >= 0; i--) {
      const ev = streamOutputs[i].event
      if (ev.type === 'assistant' && ev.content) {
        const lines = ev.content.split('\n')
        for (let j = lines.length - 1; j >= 0; j--) {
          const m = lines[j].match(/^\[(\w+)\]\s+(.+)$/)
          if (m) {
            return { id: `stream-${i}-${j}`, name: m[1], ts: streamOutputs[i].receivedAt }
          }
        }
      }
    }
    return undefined
  })

  $effect(() => {
    const id = agentId
    const cached = agentStore.agents.get(id)
    if (!cached) return
    const current = untrack(() => a)
    a = {
      ...current,
      ...cached,
      prompt: cached.prompt || current?.prompt,
      command: cached.command || current?.command,
      logPath: cached.logPath || current?.logPath,
    } as Agent
    pluginErrors = cached.pluginErrors ?? []
  })

  $effect(() => {
    const id = agentId
    pluginErrors = []
    const cached = untrack(() => agentStore.agents.get(id))
    a = cached
    if (cached) {
      pluginErrors = cached.pluginErrors ?? []
    }
    void getAgentForNode(cached?.node, id).then((data) => {
      if (agentId !== id) return
      a = data
      agentStore.updateAgent(id, data)
      pluginErrors = data.pluginErrors ?? []
    }).catch(() => { /* retained agents may expire before detail hydration */ })
  })

  $effect(() => {
    const id = agentId
    const unsubState = EventsOn(agentState(id), (data: Agent) => {
      a = data
      agentStore.updateAgent(id, data)
      pluginErrors = data.pluginErrors ?? []
    })

    const unsubError = EventsOn(agentError(id), (data: AgentErrorEvent) => {
      agentErr = data
      errorDismissed = false
    })

    const unsubEscalation = EventsOn(agentEscalation(id), (data: EscalationEvent) => {
      escalation = data
      escalationResponding = false
    })

    const unsubPluginErrors = EventsOn(agentPluginErrors(id), (data: PluginErrorsEvent) => {
      pluginErrors = data.errors ?? []
      pluginErrorsDismissed = false
    })

    return () => {
      unsubState()
      unsubError()
      unsubEscalation()
      unsubPluginErrors()
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

  {#if pluginErrors.length > 0 && !pluginErrorsDismissed}
    <div class="rounded-lg border-2 border-warning-400 bg-warning-50 p-4 dark:border-warning-600 dark:bg-warning-950">
      <div class="flex items-start gap-3">
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <span class="rounded bg-warning-200 px-2 py-0.5 text-xs font-bold text-warning-800 dark:bg-warning-700 dark:text-warning-200">
              PLUGIN ERRORS
            </span>
            <span class="text-sm font-medium text-surface-800 dark:text-surface-200">
              {pluginErrors.length} plugin{pluginErrors.length !== 1 ? 's' : ''} failed to load
            </span>
          </div>
          <ul class="mt-2 space-y-1">
            {#each pluginErrors as e}
              <li class="font-mono text-xs text-surface-600 dark:text-surface-400">{e}</li>
            {/each}
          </ul>
        </div>
        <button
          type="button"
          class="shrink-0 text-sm text-surface-400 hover:text-surface-600 dark:hover:text-surface-200"
          onclick={() => { pluginErrorsDismissed = true }}
        >
          Dismiss
        </button>
      </div>
    </div>
  {/if}

  {#if escalation}
    <div class="rounded-lg border-2 border-error-400 bg-error-50 p-4 dark:border-error-600 dark:bg-error-950">
      <div class="mb-3 flex items-center gap-2">
        <span class="rounded bg-error-200 px-2 py-0.5 text-xs font-bold text-error-800 dark:bg-error-700 dark:text-error-200">
          GUARDRAIL
        </span>
        {#if escalation.reason === 'turns'}
          <span class="text-sm font-medium text-surface-800 dark:text-surface-200">
            Assistant-event ceiling reached — {escalation.turnCount} events (limit: {escalation.limit})
          </span>
        {:else}
          <span class="text-sm font-medium text-surface-800 dark:text-surface-200">
            Post-result cost ceiling exceeded — ${escalation.costUsd?.toFixed(2)} (limit: ${escalation.limit.toFixed(2)}, source: {escalation.costSource ?? 'estimated'})
          </span>
        {/if}
      </div>
      {#if escalation.guardrail}
        <p class="mb-3 text-xs text-surface-600 dark:text-surface-300">
          {escalation.guardrail}
          {#if escalation.measuredValue !== undefined && escalation.measurement}
            {' '}breached at {escalation.measuredValue}{escalation.measurement === 'post_result_usd' ? ' USD' : ` ${escalation.measurement}`}
          {/if}
        </p>
      {/if}
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
        {timelineEntries}
        {planSteps}
        {selectedIndex}
        onselect={(i) => { selectedIndex = i }}
        {allAgents}
        {latestToolUse}
        onnavigate={onnavigate ?? (() => {})}
      />
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
