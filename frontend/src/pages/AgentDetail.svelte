<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { EventsOn } from '../../wailsjs/runtime/runtime.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import StreamOutput from '../components/StreamOutput.svelte'
  import TerminalView from '../components/TerminalView.svelte'

  interface Props {
    agentId: string
    onback: () => void
    onviewtask: (taskId: string) => void
  }

  const { agentId, onback, onviewtask }: Props = $props()

  let a = $state<agent.Agent | null>(null)
  let error = $state('')

  const isRunning = $derived(a?.state === 'running')

  $effect(() => {
    const cached = agentStore.agents.get(agentId)
    if (cached) a = cached

    const unsub = EventsOn(`agent:state:${agentId}`, (data: agent.Agent) => {
      a = data
      agentStore.updateAgent(agentId, data)
    })

    return () => {
      unsub()
    }
  })

  async function handleStop() {
    try {
      await agentStore.stop(agentId)
    } catch (e) {
      error = String(e)
    }
  }

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleString()
  }
</script>

<div class="flex flex-col gap-6 p-6">
  <button
    type="button"
    class="flex w-fit items-center gap-1 text-sm text-surface-500 hover:text-surface-800 dark:hover:text-surface-200"
    onclick={onback}
  >
    <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 19l-7-7 7-7" />
    </svg>
    Back to agents
  </button>

  {#if error}
    <p class="text-sm text-error-500">{error}</p>
  {/if}

  {#if a}
    <div class="flex flex-col gap-6">
      <div class="flex items-start justify-between gap-4">
        <div>
          <h1 class="text-2xl font-bold">{a.project || a.id}</h1>
          {#if a.name}
            <span class="text-sm text-surface-400">{a.name}</span>
          {/if}
        </div>
        <div class="flex items-center gap-2">
          <span class="inline-flex items-center gap-1 rounded-full px-3 py-1 text-sm font-medium
            {a.state === 'running' ? 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' :
             a.state === 'paused' ? 'bg-warning-200 text-warning-800 dark:bg-warning-700 dark:text-warning-200' :
             a.state === 'stopped' ? 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200' :
             'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
            {#if isRunning}
              <span class="h-2 w-2 animate-pulse rounded-full bg-success-500"></span>
            {:else if a.state === 'paused'}
              <span class="h-2 w-2 animate-pulse rounded-full bg-warning-500"></span>
            {/if}
            {a.state === 'paused' ? 'Waiting for input' : a.state}
          </span>
          {#if isRunning}
            <button
              type="button"
              class="rounded-lg bg-error-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-600"
              onclick={handleStop}
            >
              Stop
            </button>
          {/if}
        </div>
      </div>

      <div class="flex flex-wrap gap-6 text-sm">
        {#if a.taskId}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Task</span>
            <button
              type="button"
              class="text-left text-primary-500 hover:underline"
              onclick={() => onviewtask(a!.taskId)}
            >
              {a.taskId}
            </button>
          </div>
        {/if}
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Mode</span>
          <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{a.mode}</span>
        </div>
        {#if a.project}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Project</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{a.project}</span>
          </div>
        {/if}
        {#if a.name}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Session Name</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{a.name}</span>
          </div>
        {/if}
        {#if a.external}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Source</span>
            <span class="rounded bg-warning-200 px-2 py-0.5 text-warning-800 dark:bg-warning-700 dark:text-warning-200">external</span>
          </div>
        {/if}
        {#if a.pid}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">PID</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{a.pid}</span>
          </div>
        {/if}
        {#if a.command}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Command</span>
            <span class="max-w-md truncate rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{a.command}</span>
          </div>
        {/if}
        {#if a.sessionId}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Session</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{a.sessionId}</span>
          </div>
        {/if}
        {#if a.costUsd > 0}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Cost</span>
            <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">${a.costUsd.toFixed(2)}</span>
          </div>
        {/if}
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Started</span>
          <span>{formatDate(a.startedAt)}</span>
        </div>
      </div>

      <div class="flex flex-col gap-2">
        <span class="text-sm font-medium text-surface-500">Output</span>
        {#if a.mode === 'interactive' && a.tmuxSession}
          <TerminalView agentId={agentId} />
        {:else}
          <StreamOutput agentId={agentId} />
        {/if}
      </div>
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
