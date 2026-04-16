<script lang="ts">
  import { fly } from 'svelte/transition'
  import { ChevronDown, ChevronUp, PanelLeft } from '@lucide/svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { TimelineEntry } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import { convoStore } from '../../stores/convo.svelte.js'
  import ChatView from '../ChatView.svelte'
  import StreamOutput from '../StreamOutput.svelte'
  import ActionTimeline from '../ActionTimeline.svelte'
  import PlanSteps from '../PlanSteps.svelte'
  import ChatInput from '../ChatInput.svelte'

  interface Props {
    a: agent.Agent
    urgency: 'waiting' | 'required'
    planSteps: PlanStep[]
    timelineEntries: TimelineEntry[]
    selectedIndex: number | null
    onselect: (i: number) => void
  }

  const { a, urgency, planSteps, timelineEntries, selectedIndex, onselect }: Props = $props()

  let historyOpen = $state(false)
  let timelineOpen = $state(true)
  let planCollapsed = $state(false)

  const isUrgent = $derived(urgency === 'required')
  const escalationReason = $derived(a.escalationReason)

  async function handleSend(text: string) {
    await convoStore.sendMessage(a.id, text)
  }
</script>

<div class="flex flex-col gap-4">
  {#if planSteps.length > 0}
    <PlanSteps steps={planSteps} bind:collapsed={planCollapsed} />
  {/if}

  <!-- Escalation / waiting banner -->
  <div
    in:fly={{ y: 8, duration: 150 }}
    class="rounded-xl border-2 p-5
      {isUrgent
        ? 'border-error-400 bg-error-50 dark:border-error-600 dark:bg-error-950/40'
        : 'border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-900'}"
  >
    <div class="mb-4 flex items-center gap-2">
      {#if isUrgent}
        <span class="rounded-full bg-error-200 px-3 py-1 text-xs font-semibold text-error-800 dark:bg-error-700 dark:text-error-200">
          Needs your input
        </span>
        {#if escalationReason}
          <span class="text-sm text-surface-700 dark:text-surface-300">
            {escalationReason === 'turns' ? 'Turn limit reached' : 'Cost limit reached'} — agent is paused
          </span>
        {:else}
          <span class="text-sm text-surface-700 dark:text-surface-300">Agent is waiting for your response</span>
        {/if}
      {:else}
        <span class="rounded-full bg-surface-200 px-3 py-1 text-xs font-medium text-surface-600 dark:bg-surface-700 dark:text-surface-400">
          Waiting for reply
        </span>
        <span class="text-sm text-surface-500">Agent has paused and is waiting for your input</span>
      {/if}
    </div>

    <!-- Centered reply form -->
    <ChatInput
      placeholder={isUrgent ? 'What should the agent do next?' : 'Type a message...'}
      onsend={handleSend}
    />
  </div>

  <!-- Conversation history toggle -->
  <div>
    <button
      type="button"
      class="flex items-center gap-1.5 text-sm text-surface-500 hover:text-surface-700 dark:hover:text-surface-300"
      onclick={() => { historyOpen = !historyOpen }}
    >
      {#if historyOpen}
        <ChevronUp size={16} />
      {:else}
        <ChevronDown size={16} />
      {/if}
      {historyOpen ? 'Hide' : 'Show'} recent activity
    </button>

    {#if historyOpen}
      <div class="mt-3 flex min-h-0 items-start gap-3 opacity-80" in:fly={{ y: 4, duration: 120 }}>
        {#if timelineOpen}
          <div class="hidden w-64 shrink-0 overflow-hidden rounded-lg border border-surface-300 dark:border-surface-700 md:flex md:flex-col" style="max-height: 600px; min-height: 300px;">
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
          {/if}
        </div>
      </div>
    {/if}
  </div>
</div>
