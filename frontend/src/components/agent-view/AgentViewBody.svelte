<script lang="ts">
  import { fade } from 'svelte/transition'
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { task } from '../../../wailsjs/go/models.js'
  import type { AgentPhase } from '$lib/agent-phases.js'
  import type { TimestampedStreamEvent, TimelineEntry } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import QueuedLayout from './QueuedLayout.svelte'
  import RunningLayout from './RunningLayout.svelte'
  import BlockedLayout from './BlockedLayout.svelte'
  import HumanRequiredLayout from './HumanRequiredLayout.svelte'
  import ReviewingLayout from './ReviewingLayout.svelte'
  import DoneLayout from './DoneLayout.svelte'

  interface Props {
    phase: AgentPhase
    a: agent.Agent
    linkedTask?: task.Task | null
    streamOutputs: TimestampedStreamEvent[]
    convoEvents: agent.ConvoEvent[]
    timelineEntries: TimelineEntry[]
    planSteps: PlanStep[]
    selectedIndex: number | null
    onselect: (i: number) => void
  }

  const {
    phase,
    a,
    linkedTask,
    streamOutputs,
    convoEvents,
    timelineEntries,
    planSteps,
    selectedIndex,
    onselect,
  }: Props = $props()
</script>

<div class="min-h-[60vh]">
  {#key phase}
    <div in:fade={{ duration: 180 }} out:fade={{ duration: 120 }}>
      {#if phase === 'queued'}
        <QueuedLayout {linkedTask} />
      {:else if phase === 'running'}
        <RunningLayout
          {a}
          {planSteps}
          {timelineEntries}
          {selectedIndex}
          {onselect}
        />
      {:else if phase === 'blocked'}
        <BlockedLayout
          {a}
          {planSteps}
          {timelineEntries}
          {selectedIndex}
          {onselect}
        />
      {:else if phase === 'waiting' || phase === 'human-required'}
        <HumanRequiredLayout
          {a}
          urgency={phase === 'human-required' ? 'required' : 'waiting'}
          {planSteps}
          {timelineEntries}
          {selectedIndex}
          {onselect}
        />
      {:else if phase === 'reviewing'}
        <ReviewingLayout {a} {linkedTask} {streamOutputs} {convoEvents} />
      {:else}
        <DoneLayout {a} {linkedTask} {streamOutputs} {convoEvents} />
      {/if}
    </div>
  {/key}
</div>
