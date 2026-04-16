<script lang="ts">
  import type { agent } from '../../../wailsjs/go/models.js'
  import type { task } from '../../../wailsjs/go/models.js'
  import type { PhaseConfig, AgentPhase } from '$lib/agent-phases.js'

  interface Props {
    a: agent.Agent
    phase: AgentPhase
    phaseConfig: PhaseConfig
    stepText?: string
    linkedTask?: task.Task | null
    onstop: () => void
    onviewtask: (taskId: string) => void
  }

  const { a, phase, phaseConfig, stepText, linkedTask, onstop, onviewtask }: Props = $props()

  const isRunning = $derived(a.state === 'running')

  function formatDate(date: any): string {
    if (!date) return '-'
    return new Date(date).toLocaleString()
  }
</script>

<div class="flex flex-col gap-6">
  <div class="flex items-start justify-between gap-4">
    <div>
      <h1 class="text-2xl font-bold">{a.project || a.id}</h1>
      {#if a.name}
        <span class="text-sm text-surface-400">{a.name}</span>
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
      {/if}
    </div>
    <div class="flex items-center gap-2">
      <span class="inline-flex items-center gap-1 rounded-full px-3 py-1 text-sm font-medium {phaseConfig.badgeClasses}">
        {#if phaseConfig.animate}
          <span class="h-2 w-2 animate-pulse-subtle rounded-full {phaseConfig.dotClasses}"></span>
        {:else if phase !== 'queued' && phase !== 'done'}
          <span class="h-2 w-2 rounded-full {phaseConfig.dotClasses}"></span>
        {/if}
        {phaseConfig.label}
      </span>
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
          {linkedTask?.title ?? a.taskId}
        </button>
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
</div>
