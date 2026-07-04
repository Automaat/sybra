<script lang="ts">
  import type { Agent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import { EventsOn } from '$lib/api'
  import { agentState } from '../../lib/events.js'
  import StreamOutput from '../StreamOutput.svelte'
  import ChatView from '../ChatView.svelte'
  import ProviderLogo from '../ProviderLogo.svelte'

  interface Props {
    task: Task
    onviewagent: (agentId: string) => void
  }

  const { task, onviewagent }: Props = $props()

  // The running agent for this task, derived off the reactive store. Scan the
  // running set (not the most-recent agent for the task, which may be stopped
  // while an older run is still live) so the pinned live view never misses it.
  // Derived, not $state, so store updates flow straight through — this panel
  // lives *outside* the tabbed body so StreamOutput's live SSE subscription is
  // never torn down by a tab switch.
  const runningAgent = $derived(
    agentStore.byState('running').find((a) => a.taskId === task.id) ?? null,
  )
  // Key the subscription on the id alone: a same-id state update keeps this
  // value stable, so the effect does not churn unsubscribe/resubscribe on every
  // event — it only re-subscribes when a different agent starts running.
  const runningAgentId = $derived(runningAgent?.id)

  $effect(() => {
    const id = runningAgentId
    if (!id) return
    const unsub = EventsOn(agentState(id), (data: Agent) => {
      agentStore.updateAgent(data.id, data)
    })
    return () => { unsub() }
  })
</script>

{#if runningAgent}
  <div class="flex flex-col gap-3">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2">
        <span class="text-sm font-medium text-surface-500">Agent</span>
        {#if runningAgent.provider}
          <ProviderLogo provider={runningAgent.provider} class="h-4 w-4 text-surface-500" />
        {/if}
        <button
          type="button"
          class="font-mono text-sm text-primary-500 hover:underline"
          onclick={() => onviewagent(runningAgent!.id)}
        >
          {runningAgent.id}
        </button>
        <span
          class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium {runningAgent.state === 'running' ? 'bg-primary-200 text-primary-800 dark:bg-primary-700 dark:text-primary-200' : 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}"
        >
          {#if runningAgent.state === 'running'}
            <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
          {/if}
          {runningAgent.state}
        </span>
      </div>
      {#if runningAgent.state === 'running'}
        <button
          type="button"
          class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600"
          onclick={() => agentStore.stop(runningAgent!.id)}
        >
          Stop
        </button>
      {/if}
    </div>
    {#if runningAgent.prompt}
      <details class="rounded-lg border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-800">
        <summary class="cursor-pointer select-none px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Prompt</summary>
        <pre class="max-h-64 overflow-y-auto whitespace-pre-wrap border-t border-surface-300 px-3 py-2 text-xs text-surface-700 dark:border-surface-600 dark:text-surface-300">{runningAgent.prompt}</pre>
      </details>
    {/if}
    {#if runningAgent.mode === 'interactive'}
      <ChatView
        agentId={runningAgent.id}
        agentState={runningAgent.state}
        costUsd={runningAgent.costUsd}
        inputTokens={runningAgent.inputTokens ?? 0}
        outputTokens={runningAgent.outputTokens ?? 0}
        bounded={true}
      />
    {:else}
      <StreamOutput agentId={runningAgent.id} />
    {/if}
  </div>
{/if}
