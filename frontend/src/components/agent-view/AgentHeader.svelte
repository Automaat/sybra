<script lang="ts">
  import type { Agent } from '../../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import type { AgentPhase } from '$lib/agent-phases.js'
  import { agentStore } from '../../stores/agents.svelte.js'
  import AgentStateBadge from '../AgentStateBadge.svelte'
  import { formatDateTime } from '$lib/dates.js'
  import { agentDisplayName, cleanAgentName, shortId } from '$lib/agent-name.js'

  interface Props {
    a: Agent
    phase: AgentPhase
    stepText?: string
    linkedTask?: Task | null
    onstop: () => void
    onviewtask: (taskId: string) => void
  }

  const { a, phase, stepText, linkedTask, onstop, onviewtask }: Props = $props()

  const isRunning = $derived(a.state === 'running')
  const heading = $derived(agentDisplayName(a, linkedTask?.title))
  // Show the session name only when it adds something beyond the heading.
  const subtitle = $derived(cleanAgentName(a.name))
  const showSubtitle = $derived(subtitle.length > 0 && subtitle !== heading)
  const queueInfo = $derived(a.taskId ? agentStore.queueByTask?.get(a.taskId) : undefined)
</script>

<div class="flex flex-col gap-6">
  <div class="flex items-start justify-between gap-4">
    <div class="min-w-0">
      <div class="flex flex-wrap items-center gap-2">
        <h1 class="break-words text-2xl font-bold">{heading}</h1>
        <span class="rounded bg-surface-200 px-1.5 py-0.5 font-mono text-[10px] text-surface-500 dark:bg-surface-700 dark:text-surface-400" title="Agent ID: {a.id}">{shortId(a.id)}</span>
      </div>
      {#if showSubtitle}
        <span class="text-sm text-surface-400">{subtitle}</span>
      {/if}
      {#if phase === 'running'}
        <p class="mt-0.5 text-sm italic text-surface-400">
          {stepText ?? 'Working...'}
        </p>
      {:else if phase === 'waiting'}
        <p class="mt-0.5 text-sm text-surface-400">Waiting for reply</p>
      {:else if phase === 'blocked'}
        <p class="mt-0.5 text-sm text-tertiary-600 dark:text-tertiary-400">
          Awaiting tool approval
        </p>
      {:else if phase === 'human-required'}
        <p class="mt-0.5 text-sm font-medium text-error-600 dark:text-error-400">
          Waiting for human input
        </p>
      {:else if phase === 'reviewing'}
        <p class="mt-0.5 text-sm text-warning-600 dark:text-warning-400">
          Under review
        </p>
      {:else if phase === 'queued'}
        <p class="mt-0.5 text-sm text-surface-400">
          {#if queueInfo}
            Waiting for a slot · Queue {queueInfo.position} of {queueInfo.depth}
          {:else}
            Waiting for a slot
          {/if}
        </p>
      {/if}
    </div>
    <div class="flex items-center gap-2">
      <AgentStateBadge {phase} size="md" />
      {#if isRunning}
        <button
          type="button"
          class="rounded-lg bg-error-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-error-600"
          onclick={onstop}
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
          onclick={() => onviewtask(a.taskId)}
        >
          View task →
        </button>
      </div>
    {/if}
    {#if queueInfo}
      <div class="flex flex-col gap-1">
        <span class="font-medium text-surface-500">Queue</span>
        <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{queueInfo.position} of {queueInfo.depth}</span>
      </div>
    {/if}
    {#if linkedTask?.branch}
      <div class="flex flex-col gap-1">
        <span class="font-medium text-surface-500">Branch</span>
        <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{linkedTask.branch}</span>
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
      <span>{formatDateTime(a.startedAt)}</span>
    </div>
  </div>
</div>
