<script lang="ts">
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import { agentStore } from '../stores/agents.svelte.js'
  import AgentCard from '../components/AgentCard.svelte'
  import { EventsOn } from '$lib/api'
  import { agentOutput } from '../lib/events.js'
  import { extractStepText } from '$lib/step-text.js'
  import type { agent } from '../../wailsjs/go/models.js'

  interface Props {
    onselect: (id: string) => void
  }

  const { onselect }: Props = $props()

  let filter = $state('all')

  // Subscribe to output events for all running agents so AgentCard step text
  // stays live even when StreamOutput (AgentDetail) is not mounted.
  $effect(() => {
    const runningIds = agentStore.list
      .filter((a) => a.state === 'running')
      .map((a) => a.id)
    const unsubs = runningIds.map((id) =>
      EventsOn(agentOutput(id), (event: agent.StreamEvent) => {
        const text = extractStepText(event)
        if (text) agentStore.setStepText(id, text)
      }),
    )
    return () => unsubs.forEach((u) => u())
  })

  const states = [
    { value: 'all', label: 'All' },
    { value: 'running', label: 'Running' },
    { value: 'paused', label: 'Paused' },
    { value: 'idle', label: 'Idle' },
    { value: 'stopped', label: 'Stopped' },
  ]

  const filtered = $derived(agentStore.byState(filter))
</script>

<div class="flex flex-col gap-3 p-4 md:gap-4 md:p-6">
  <SegmentedControl orientation="horizontal" value={filter} onValueChange={(details) => (filter = details.value ?? 'all')}>
    <SegmentedControl.Control>
      <SegmentedControl.Indicator />
      {#each states as s}
        <SegmentedControl.Item value={s.value}>
          <SegmentedControl.ItemText>{s.label}</SegmentedControl.ItemText>
          <SegmentedControl.ItemHiddenInput />
        </SegmentedControl.Item>
      {/each}
    </SegmentedControl.Control>
  </SegmentedControl>

  {#if agentStore.loading}
    <p class="text-center text-sm opacity-60">Loading agents...</p>
  {:else if agentStore.error}
    <p class="text-center text-sm text-error-500">{agentStore.error}</p>
  {:else if filtered.length === 0}
    <div class="flex flex-col items-center gap-2 py-16 opacity-50">
      <p class="text-lg">No agents</p>
      <p class="text-sm">Start an agent from a task to see it here</p>
    </div>
  {:else}
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {#each filtered as a (a.id)}
        <AgentCard agent={a} onclick={() => onselect(a.id)} />
      {/each}
    </div>
  {/if}
</div>
