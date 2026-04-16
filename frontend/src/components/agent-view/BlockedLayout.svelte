<script lang="ts">
  import { fly } from 'svelte/transition'
  import { ChevronDown, ChevronUp, PanelLeft } from '@lucide/svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { TimelineEntry } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import { convoStore } from '../../stores/convo.svelte.js'
  import ToolApproval from '../ToolApproval.svelte'
  import ChatView from '../ChatView.svelte'
  import StreamOutput from '../StreamOutput.svelte'
  import ActionTimeline from '../ActionTimeline.svelte'
  import PlanSteps from '../PlanSteps.svelte'

  interface Props {
    a: agent.Agent
    planSteps: PlanStep[]
    timelineEntries: TimelineEntry[]
    selectedIndex: number | null
    onselect: (i: number) => void
  }

  const { a, planSteps, timelineEntries, selectedIndex, onselect }: Props = $props()

  let historyOpen = $state(false)
  let timelineOpen = $state(true)
  let planCollapsed = $state(false)

  const approvals = $derived([...convoStore.pendingApprovals.values()])

  async function handleApproval(toolUseId: string, approved: boolean) {
    await convoStore.respondApproval(toolUseId, approved)
  }
</script>

<div class="flex flex-col gap-4">
  {#if planSteps.length > 0}
    <PlanSteps steps={planSteps} bind:collapsed={planCollapsed} />
  {/if}

  <!-- Hoisted approval cards — full width, prominent -->
  {#if approvals.length > 0}
    <div in:fly={{ y: 8, duration: 150 }} class="flex flex-col gap-3">
      <div class="flex items-center gap-2">
        <span class="rounded-full bg-tertiary-200 px-3 py-1 text-xs font-semibold text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
          Needs your response
        </span>
        <span class="text-xs text-surface-500">{approvals.length} pending {approvals.length === 1 ? 'approval' : 'approvals'}</span>
      </div>
      {#each approvals as approval (approval.toolUseId)}
        <ToolApproval
          toolUseId={approval.toolUseId}
          toolName={approval.toolName}
          input={approval.input}
          onrespond={handleApproval}
        />
      {/each}
    </div>
  {:else}
    <p class="text-sm text-surface-400">Waiting for tool approval...</p>
  {/if}

  <!-- Chat history — collapsed by default when blocked -->
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
      {historyOpen ? 'Hide' : 'Show'} conversation history
    </button>

    {#if historyOpen}
      <div class="mt-3 flex min-h-0 items-start gap-3 opacity-75" in:fly={{ y: 4, duration: 120 }}>
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
              suppressApprovals={true}
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
