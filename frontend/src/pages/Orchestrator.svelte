<script lang="ts">
  import {
    StartOrchestrator,
    StopOrchestrator,
    IsOrchestratorRunning,
    GetOrchestratorAgentID,
    GetMonitorReport,
    EventsOn,
  } from '$lib/api'
  import type { agent, monitor, sybra } from '../../wailsjs/go/models.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import { convoStore } from '../stores/convo.svelte.js'
  import { MonitorReport, OrchestratorState } from '../lib/events.js'
  import MessageBubble from '../components/MessageBubble.svelte'
  import StreamOutput from '../components/StreamOutput.svelte'

  let running = $state(false)
  let orchestratorId = $state('')
  let events = $state<agent.ConvoEvent[]>([])
  let error = $state('')
  let container: HTMLDivElement | undefined = $state()
  let convoUnsub: (() => void) | undefined
  let monitorBinding = $state<sybra.MonitorReportBinding | null>(null)
  let lastReportAt = $state<number | null>(null)
  let monitorTick = $state(0)

  const monitorReport = $derived<monitor.Report | null>(
    monitorBinding?.ready ? monitorBinding.report : null
  )
  const driftCount = $derived(monitorReport?.anomalies?.length ?? 0)

  const lastTickAge = $derived.by(() => {
    void monitorTick
    if (lastReportAt === null) return null
    return Math.max(0, Math.floor((Date.now() - lastReportAt) / 1000))
  })

  function formatAge(sec: number): string {
    if (sec < 60) return `${sec}s`
    if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`
    return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`
  }

  function recordReport(binding: sybra.MonitorReportBinding | null) {
    monitorBinding = binding
    if (binding && binding.ready) {
      const t = new Date(binding.report.generatedAt as unknown as string).getTime()
      lastReportAt = Number.isNaN(t) ? Date.now() : t
    }
  }

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

  async function attachConvo(id: string) {
    orchestratorId = id
    events = (await convoStore.getOutput(id)) ?? []
    requestAnimationFrame(scrollToBottom)
    convoUnsub?.()
    convoUnsub = convoStore.subscribe(id)
  }

  function detachConvo() {
    convoUnsub?.()
    convoUnsub = undefined
    orchestratorId = ''
    events = []
  }

  async function checkStatus() {
    running = await IsOrchestratorRunning()
    if (running) {
      const id = await GetOrchestratorAgentID()
      if (id) await attachConvo(id)
    }
  }

  async function handleStart() {
    try {
      error = ''
      await StartOrchestrator()
      running = true
      const id = await GetOrchestratorAgentID()
      if (id) await attachConvo(id)
    } catch (e) {
      error = String(e)
    }
  }

  async function handleStop() {
    try {
      error = ''
      await StopOrchestrator()
      running = false
      detachConvo()
    } catch (e) {
      error = String(e)
    }
  }

  $effect(() => {
    checkStatus()
    GetMonitorReport().then((b) => {
      recordReport(b)
    })

    const unsub = EventsOn(OrchestratorState, async (state: string) => {
      running = state === 'running'
      if (running) {
        const id = await GetOrchestratorAgentID()
        if (id) await attachConvo(id)
      } else {
        detachConvo()
      }
    })

    const unsubMonitor = EventsOn(MonitorReport, (report: monitor.Report) => {
      recordReport({
        enabled: true,
        ready: true,
        report,
      } as sybra.MonitorReportBinding)
    })

    // Tick the derived age every second so the UI counts up without waiting
    // for a new monitor tick event.
    const ageTimer = window.setInterval(() => {
      monitorTick++
    }, 1000)

    return () => {
      unsub()
      unsubMonitor()
      window.clearInterval(ageTimer)
      convoUnsub?.()
    }
  })

  // Pick up new events from the reactive convo store.
  $effect(() => {
    if (!orchestratorId) return
    const current = convoStore.conversations.get(orchestratorId)
    if (current && current.length > events.length) {
      events = current
      requestAnimationFrame(scrollToBottom)
    }
  })
</script>

<div class="flex h-full min-h-0 flex-col gap-3 overflow-hidden p-4 md:gap-4 md:p-6">
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
        {#if monitorBinding && monitorBinding.enabled}
          <span class="text-xs {driftCount > 0 ? 'text-warning-500' : 'text-surface-500'}">
            {#if !monitorBinding.ready}
              monitor: waiting
            {:else if lastTickAge !== null}
              monitor: {formatAge(lastTickAge)} ago
              {#if driftCount > 0}
                <span class="text-warning-500">· drift={driftCount}</span>
              {/if}
            {:else}
              monitor: ready
            {/if}
          </span>
        {/if}
      </div>
      <div class="flex gap-2">
        {#if running}
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
      <div
        bind:this={container}
        class="flex max-h-[50dvh] md:max-h-[400px] flex-col gap-2 overflow-y-auto rounded-lg border border-surface-300 bg-surface-900 p-3 text-xs text-surface-200 dark:border-surface-600"
      >
        {#if events.length === 0}
          <p class="text-surface-500">Waiting for orchestrator…</p>
        {:else}
          {#each events as event, i (i)}
            <MessageBubble {event} />
          {/each}
        {/if}
      </div>
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
