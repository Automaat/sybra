<script lang="ts">
  import { PanelLeft } from '@lucide/svelte'
  import type { Agent, ConvoEvent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import type { TimelineEntry } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import type { TimestampedStreamEvent } from '$lib/timeline.js'
  import type { ToolUseSignal } from '$lib/workspace-tabs.js'
  import StreamOutput from '../StreamOutput.svelte'
  import ChatView from '../ChatView.svelte'
  import ChatInput from '../ChatInput.svelte'
  import ActionTimeline from '../ActionTimeline.svelte'
  import ThreePanelLayout from './ThreePanelLayout.svelte'
  import SessionWorkspace from './SessionWorkspace.svelte'
  import AgentSidebarList from './AgentSidebarList.svelte'
  import { convoStore } from '../../stores/convo.svelte.js'

  interface Props {
    a: Agent
    planSteps: PlanStep[]
    timelineEntries: TimelineEntry[]
    selectedIndex: number | null
    onselect: (i: number) => void
    streamOutputs: TimestampedStreamEvent[]
    convoEvents: ConvoEvent[]
    allAgents: Agent[]
    latestToolUse: ToolUseSignal | undefined
    onnavigate: (id: string) => void
  }

  const {
    a,
    planSteps,
    timelineEntries,
    selectedIndex,
    onselect,
    streamOutputs,
    convoEvents,
    allAgents,
    latestToolUse,
    onnavigate,
  }: Props = $props()

  let timelineOpen = $state(true)

  // The backend reports whether this run actually has a live stdin transport
  // (Agent.CanSteer), gated on the same transport SendMessage requires.
  // Deriving steerability from mode/provider/state would wrongly show controls
  // for a rollback-disabled (headless_steerable=false) or legacy-reattached
  // headless run that has no stdin transport, then fail every send.
  const isSteerableHeadless = $derived(a.canSteer)
  let pauseError = $state<string | null>(null)

  async function handleSend(text: string) {
    await convoStore.sendMessage(a.id, text)
  }

  async function handlePause() {
    pauseError = null
    try {
      await handleSend('Please pause your current work and wait for further instructions before continuing.')
    } catch (e) {
      pauseError = e instanceof Error ? e.message : 'Failed to send pause instruction'
    }
  }
</script>

<ThreePanelLayout>
  {#snippet sidebar()}
    <AgentSidebarList agents={allAgents} activeId={a.id} {onnavigate} />
  {/snippet}

  <!-- Center: timeline + output stream -->
  <div class="flex min-h-0 flex-col gap-2">
    <div class="flex items-center gap-2">
      <span class="text-sm font-medium text-surface-500">Output</span>
      <button
        type="button"
        title={timelineOpen ? 'Hide timeline' : 'Show timeline'}
        onclick={() => { timelineOpen = !timelineOpen }}
        class="ml-auto flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-medium transition-colors
          {timelineOpen
            ? 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200'
            : 'bg-surface-200 text-surface-500 dark:bg-surface-700 dark:text-surface-400'}"
      >
        <PanelLeft size={12} />
        Timeline
      </button>
    </div>
    <div class="flex min-h-0 items-start gap-3">
      {#if timelineOpen}
        <div class="hidden w-56 shrink-0 overflow-hidden rounded-lg border border-surface-300 dark:border-surface-700 md:flex md:flex-col" style="max-height: 600px; min-height: 300px;">
          <ActionTimeline
            entries={timelineEntries}
            activeIndex={selectedIndex}
            onselect={(i) => { onselect(i) }}
          />
        </div>
      {/if}
      <div class="min-w-0 flex-1">
        {#if a.mode === 'interactive'}
          <ChatView
            agentId={a.id}
            agentState={a.state}
            costUsd={a.costUsd}
            inputTokens={a.inputTokens ?? 0}
            outputTokens={a.outputTokens ?? 0}
            highlightIndex={selectedIndex}
            onvisibleindex={(i) => { if (selectedIndex === null) onselect(i) }}
          />
        {:else}
          <StreamOutput
            agentId={a.id}
            highlightIndex={selectedIndex}
            onvisibleindex={(i) => { if (selectedIndex === null) onselect(i) }}
          />
          {#if isSteerableHeadless}
            <div class="mt-2 rounded-lg border border-surface-300 dark:border-surface-700">
              <div class="flex items-center justify-between px-3 pt-2">
                <span class="text-xs font-medium text-surface-500">Steer agent</span>
                <button
                  type="button"
                  title="Send a cooperative pause instruction — the agent finishes its current step, then waits"
                  onclick={handlePause}
                  class="rounded px-1.5 py-0.5 text-[10px] font-medium text-surface-500 transition-colors
                    hover:bg-surface-200 dark:hover:bg-surface-700"
                >
                  Pause
                </button>
              </div>
              {#if pauseError}
                <p class="px-3 pt-1 text-xs text-error-600 dark:text-error-400">{pauseError}</p>
              {/if}
              <ChatInput
                placeholder="Send guidance to the running agent..."
                onsend={handleSend}
              />
            </div>
          {/if}
        {/if}
      </div>
    </div>
  </div>

  {#snippet workspace()}
    <SessionWorkspace
      agentId={a.id}
      taskId={a.taskId}
      {streamOutputs}
      {convoEvents}
      {planSteps}
      {latestToolUse}
    />
  {/snippet}
</ThreePanelLayout>
