<script lang="ts">
  import {
    StartOrchestrator,
    StopOrchestrator,
    IsOrchestratorRunning,
    CaptureOrchestratorPane,
    AttachOrchestrator,
  } from '../../wailsjs/go/main/App.js'
  import { EventsOn } from '../../wailsjs/runtime/runtime.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { OrchestratorState } from '../lib/events.js'
  import StreamOutput from '../components/StreamOutput.svelte'

  let running = $state(false)
  let output = $state('')
  let error = $state('')
  let container: HTMLPreElement | undefined = $state()

  const triageAgents = $derived(
    agentStore.list.filter((a) => a.name?.startsWith('triage:'))
  )

  const runningTriageCount = $derived(
    triageAgents.filter((a) => a.state === 'running').length
  )

  const evalAgents = $derived(
    agentStore.list.filter((a) => a.name?.startsWith('eval:'))
  )

  const runningEvalCount = $derived(
    evalAgents.filter((a) => a.state === 'running').length
  )

  function scrollToBottom() {
    if (container) {
      container.scrollTop = container.scrollHeight
    }
  }

  async function checkStatus() {
    running = await IsOrchestratorRunning()
  }

  async function poll() {
    if (!running) return
    try {
      const text = await CaptureOrchestratorPane()
      if (text !== output) {
        output = text
        requestAnimationFrame(scrollToBottom)
      }
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function handleStart() {
    try {
      error = ''
      await StartOrchestrator()
      running = true
      output = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function handleStop() {
    try {
      error = ''
      await StopOrchestrator()
      running = false
    } catch (e) {
      error = String(e)
    }
  }

  async function handleAttach() {
    try {
      await AttachOrchestrator()
    } catch (e) {
      error = String(e)
    }
  }

  $effect(() => {
    checkStatus()

    const unsub = EventsOn(OrchestratorState, (state: string) => {
      running = state === 'running'
      if (!running) output = ''
    })

    const timer = setInterval(() => {
      poll()
    }, 1000)

    return () => {
      unsub()
      clearInterval(timer)
    }
  })
</script>

<div class="flex h-full flex-col gap-4 overflow-hidden p-6">
  <!-- Orchestrator Session -->
  <section class="flex flex-col gap-3">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-3">
        <h3 class="text-sm font-semibold text-surface-200">Interactive Session</h3>
        <div
          class="h-2.5 w-2.5 rounded-full {running ? 'bg-success-500 animate-pulse' : 'bg-surface-500'}"
        ></div>
        <span class="text-xs text-surface-400">
          {running ? 'Running' : 'Stopped'}
        </span>
      </div>
      <div class="flex gap-2">
        {#if running}
          <button
            type="button"
            class="rounded-lg bg-primary-500 px-3 py-1.5 text-xs font-medium text-white hover:bg-primary-600"
            onclick={handleAttach}
          >
            Attach
          </button>
          <button
            type="button"
            class="rounded-lg bg-error-500 px-3 py-1.5 text-xs font-medium text-white hover:bg-error-600"
            onclick={handleStop}
          >
            Stop
          </button>
        {:else}
          <button
            type="button"
            class="rounded-lg bg-success-500 px-3 py-1.5 text-xs font-medium text-white hover:bg-success-600"
            onclick={handleStart}
          >
            Start
          </button>
        {/if}
      </div>
    </div>

    {#if error}
      <p class="text-xs text-error-500">{error}</p>
    {/if}

    {#if running}
      <pre
        bind:this={container}
        class="max-h-[300px] overflow-y-auto whitespace-pre-wrap rounded-lg border border-surface-300 bg-surface-900 p-3 font-mono text-xs text-surface-200 dark:border-surface-600"
      >{output || 'Waiting for output...'}</pre>
    {/if}
  </section>

  <!-- Triage Agents -->
  <section class="flex min-h-0 flex-1 flex-col gap-3">
    <div class="flex items-center gap-3">
      <h3 class="text-sm font-semibold text-surface-200">Triage Agents</h3>
      {#if runningTriageCount > 0}
        <span class="rounded-full bg-primary-500/20 px-2 py-0.5 text-xs font-medium text-primary-400">
          {runningTriageCount} running
        </span>
      {/if}
    </div>

    {#if triageAgents.length === 0}
      <p class="py-4 text-center text-xs text-surface-500">
        No triage sessions yet. Create a task to trigger auto-triage.
      </p>
    {:else}
      <div class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto">
        {#each triageAgents as ta (ta.id)}
          <div class="rounded-lg border border-surface-300 bg-surface-800 dark:border-surface-600">
            <div class="flex items-center justify-between border-b border-surface-700 px-3 py-2">
              <div class="flex items-center gap-2">
                <div
                  class="h-2 w-2 rounded-full {ta.state === 'running' ? 'bg-success-500 animate-pulse' : ta.state === 'stopped' ? 'bg-surface-500' : 'bg-warning-500'}"
                ></div>
                <span class="text-xs font-medium text-surface-200">
                  {ta.name?.replace('triage:', '') || ta.taskId}
                </span>
              </div>
              <div class="flex items-center gap-2">
                {#if ta.costUsd > 0}
                  <span class="text-xs text-surface-400">${ta.costUsd.toFixed(4)}</span>
                {/if}
                <span class="rounded bg-surface-700 px-1.5 py-0.5 text-[10px] font-medium text-surface-300">
                  {ta.state}
                </span>
              </div>
            </div>
            <div class="p-2">
              <StreamOutput agentId={ta.id} />
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </section>

  <!-- Eval Agents -->
  <section class="flex min-h-0 flex-1 flex-col gap-3">
    <div class="flex items-center gap-3">
      <h3 class="text-sm font-semibold text-surface-200">Eval Agents</h3>
      {#if runningEvalCount > 0}
        <span class="rounded-full bg-warning-500/20 px-2 py-0.5 text-xs font-medium text-warning-400">
          {runningEvalCount} running
        </span>
      {/if}
    </div>

    {#if evalAgents.length === 0}
      <p class="py-4 text-center text-xs text-surface-500">
        No evaluations yet. Agents trigger eval on completion.
      </p>
    {:else}
      <div class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto">
        {#each evalAgents as ea (ea.id)}
          <div class="rounded-lg border border-surface-300 bg-surface-800 dark:border-surface-600">
            <div class="flex items-center justify-between border-b border-surface-700 px-3 py-2">
              <div class="flex items-center gap-2">
                <div
                  class="h-2 w-2 rounded-full {ea.state === 'running' ? 'bg-warning-500 animate-pulse' : ea.state === 'stopped' ? 'bg-surface-500' : 'bg-warning-500'}"
                ></div>
                <span class="text-xs font-medium text-surface-200">
                  {ea.name?.replace('eval:', '') || ea.taskId}
                </span>
              </div>
              <div class="flex items-center gap-2">
                {#if ea.costUsd > 0}
                  <span class="text-xs text-surface-400">${ea.costUsd.toFixed(4)}</span>
                {/if}
                <span class="rounded bg-surface-700 px-1.5 py-0.5 text-[10px] font-medium text-surface-300">
                  {ea.state}
                </span>
              </div>
            </div>
            <div class="p-2">
              <StreamOutput agentId={ea.id} />
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </section>
</div>
