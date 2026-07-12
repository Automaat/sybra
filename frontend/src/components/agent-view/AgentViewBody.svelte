<script lang="ts">
  import { fade } from 'svelte/transition'
  import type { Agent, ConvoEvent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import type { AgentPhase } from '$lib/agent-phases.js'
  import type { TimestampedStreamEvent, TimelineEntry } from '$lib/timeline.js'
  import type { PlanStep } from '$lib/plan-steps.js'
  import type { ToolUseSignal } from '$lib/workspace-tabs.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import QueuedLayout from './QueuedLayout.svelte'
  import RunningLayout from './RunningLayout.svelte'
  import BlockedLayout from './BlockedLayout.svelte'
  import HumanRequiredLayout from './HumanRequiredLayout.svelte'
  import ReviewingLayout from './ReviewingLayout.svelte'
  import DoneLayout from './DoneLayout.svelte'

  interface Props {
    phase: AgentPhase
    a: Agent
    linkedTask?: Task | null
    streamOutputs: TimestampedStreamEvent[]
    convoEvents: ConvoEvent[]
    timelineEntries: TimelineEntry[]
    planSteps: PlanStep[]
    selectedIndex: number | null
    onselect: (i: number) => void
    // Three-panel props — only consumed by active-phase layouts.
    allAgents: Agent[]
    latestToolUse: ToolUseSignal | undefined
    onnavigate: (id: string) => void
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
    allAgents,
    latestToolUse,
    onnavigate,
  }: Props = $props()

  const queueInfo = $derived(a.taskId ? agentStore.queueByTask.get(a.taskId) : null)
</script>

<div class="min-h-[60vh]">
  {#key phase}
    <div in:fade={{ duration: 180 }} out:fade={{ duration: 120 }}>
      {#if phase === 'queued'}
        <QueuedLayout {linkedTask} {queueInfo} />
      {:else if phase === 'running'}
        <RunningLayout
          {a}
          {planSteps}
          {timelineEntries}
          {selectedIndex}
          {onselect}
          {streamOutputs}
          {convoEvents}
          {allAgents}
          {latestToolUse}
          {onnavigate}
        />
      {:else if phase === 'blocked'}
        <BlockedLayout
          {a}
          {planSteps}
          {timelineEntries}
          {selectedIndex}
          {onselect}
          {streamOutputs}
          {convoEvents}
          {allAgents}
          {latestToolUse}
          {onnavigate}
        />
      {:else if phase === 'waiting' || phase === 'human-required'}
        <HumanRequiredLayout
          {a}
          urgency={phase === 'human-required' ? 'required' : 'waiting'}
          {planSteps}
          {timelineEntries}
          {selectedIndex}
          {onselect}
          {streamOutputs}
          {convoEvents}
          {allAgents}
          {latestToolUse}
          {onnavigate}
        />
      {:else if phase === 'reviewing'}
        <ReviewingLayout {a} {linkedTask} {streamOutputs} {convoEvents} />
      {:else}
        <DoneLayout {a} {linkedTask} {streamOutputs} {convoEvents} />
      {/if}
    </div>
  {/key}
</div>
