<script lang="ts">
  import { PanelLeft } from '@lucide/svelte'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { TimelineEntry } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import StreamOutput from '../StreamOutput.svelte'
  import ChatView from '../ChatView.svelte'
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

  let timelineOpen = $state(true)
  let planCollapsed = $state(false)
</script>

<div class="flex flex-col gap-4">
  {#if planSteps.length > 0}
    <PlanSteps steps={planSteps} bind:collapsed={planCollapsed} />
  {/if}

  <div class="flex min-h-0 flex-1 flex-col gap-2">
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
  </div>
</div>
