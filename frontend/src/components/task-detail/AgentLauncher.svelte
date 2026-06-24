<script lang="ts">
  import type { Agent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import { connectionStore } from '../../stores/connection.svelte.js'
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

  let runningAgent = $state<Agent | null>(null)
  let prompt = $state('')
  let agentMode = $state('interactive')
  let includeTaskDescription = $state(false)
  let starting = $state(false)
  let error = $state('')
  let modeInit = false

  $effect(() => {
    if (!modeInit && task.agentMode) {
      agentMode = task.agentMode
      modeInit = true
    }
  })

  $effect(() => {
    const existing = agentStore.byTask(task.id)
    if (existing && existing.state === 'running') {
      runningAgent = existing
    }
  })

  $effect(() => {
    if (!runningAgent) return
    const id = runningAgent.id
    const unsub = EventsOn(agentState(id), (data: Agent) => {
      runningAgent = data
      agentStore.updateAgent(data.id, data)
    })
    return () => { unsub() }
  })

  async function startAgent() {
    if (!prompt.trim()) return
    starting = true
    error = ''
    try {
      runningAgent = await agentStore.start(task.id, agentMode, prompt.trim(), includeTaskDescription)
      prompt = ''
    } catch (e) {
      error = String(e)
    } finally {
      starting = false
    }
  }
</script>

{#if error}
  <p class="text-xs text-error-500">{error}</p>
{/if}

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
{:else if task.status !== 'plan-review'}
  <!-- A plan-review task's job is to approve/reject the plan, not launch a new
       ad-hoc run, so the generic "new run" form is collapsed for it. The live
       running-agent view above still always shows. -->
  <div class="flex flex-col gap-3">
    <div class="flex flex-col gap-0.5">
      <span class="text-sm font-medium text-surface-500">New agent run</span>
      <span class="text-xs text-surface-400">
        Starts a fresh agent with the prompt below — separate from resuming the
        Claude Code session that produced a PR.
      </span>
    </div>
    <div class="flex flex-wrap items-center gap-4">
      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          checked={agentMode === 'headless'}
          onchange={(e) => { agentMode = e.currentTarget.checked ? 'headless' : 'interactive' }}
          class="rounded border-surface-300 dark:border-surface-600"
        />
        <span class="text-sm">Headless</span>
      </label>
      <label class="flex items-center gap-2">
        <input
          type="checkbox"
          checked={includeTaskDescription}
          onchange={(e) => { includeTaskDescription = e.currentTarget.checked }}
          class="rounded border-surface-300 dark:border-surface-600"
        />
        <span class="text-sm">Include task description</span>
      </label>
    </div>
    <textarea
      class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
      rows="3"
      placeholder="Enter prompt for the agent..."
      bind:value={prompt}
    ></textarea>
    <div class="flex items-center gap-2">
      <button
        type="button"
        class="w-fit rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-50"
        onclick={startAgent}
        disabled={starting || !prompt.trim() || !connectionStore.online}
        title={!connectionStore.online ? 'Offline — agent cannot start until connection is restored' : undefined}
      >
        {starting ? 'Starting...' : 'Start agent'}
      </button>
      {#if !connectionStore.online}
        <span class="text-xs text-warning-600 dark:text-warning-400">Offline</span>
      {/if}
    </div>
  </div>
{/if}
