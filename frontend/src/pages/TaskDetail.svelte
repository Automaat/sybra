<script lang="ts">
  import { SegmentedControl } from '@skeletonlabs/skeleton-svelte'
  import type { agent, task } from '../../wailsjs/go/models.js'
  import { EventsOn } from '../../wailsjs/runtime/runtime.js'
  import { taskStore } from '../stores/tasks.svelte.js'
  import { agentStore } from '../stores/agents.svelte.js'
  import StatusBadge from '../components/StatusBadge.svelte'
  import StreamOutput from '../components/StreamOutput.svelte'
  import TerminalView from '../components/TerminalView.svelte'

  interface Props {
    taskId: string
    onback: () => void
    onviewagent: (agentId: string) => void
    ondelete: () => void
  }

  const { taskId, onback, onviewagent, ondelete }: Props = $props()

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') onback()
  }

  $effect(() => {
    window.addEventListener('keydown', handleKeydown)
    return () => window.removeEventListener('keydown', handleKeydown)
  })

  let deleting = $state(false)

  let t = $state<task.Task | null>(null)
  let error = $state('')
  let prompt = $state('')
  let agentMode = $state('interactive')
  let starting = $state(false)
  let runningAgent = $state<agent.Agent | null>(null)

  const statusOptions = [
    { value: 'new', label: 'New' },
    { value: 'todo', label: 'Todo' },
    { value: 'planning', label: 'Planning' },
    { value: 'plan-review', label: 'Plan Review' },
    { value: 'in-progress', label: 'In Progress' },
    { value: 'in-review', label: 'In Review' },
    { value: 'human-required', label: 'Human Required' },
    { value: 'done', label: 'Done' },
  ]

  $effect(() => {
    loadTask()
    const existing = agentStore.byTask(taskId)
    if (existing && existing.state === 'running') {
      runningAgent = existing
    }
  })

  $effect(() => {
    if (!runningAgent) return
    const unsub = EventsOn(`agent:state:${runningAgent.id}`, (data: agent.Agent) => {
      runningAgent = data
      agentStore.updateAgent(data.id, data)
    })
    return () => { unsub() }
  })

  async function loadTask() {
    try {
      t = await taskStore.get(taskId)
      agentMode = t.agentMode || 'interactive'
    } catch (e) {
      error = String(e)
    }
  }

  async function updateStatus(value: string) {
    if (!t || t.status === value) return
    try {
      t = await taskStore.update(taskId, { status: value })
    } catch (e) {
      error = String(e)
    }
  }

  async function startAgent() {
    if (!t || !prompt.trim()) return
    starting = true
    error = ''
    try {
      runningAgent = await agentStore.start(taskId, agentMode, prompt.trim())
      prompt = ''
    } catch (e) {
      error = String(e)
    } finally {
      starting = false
    }
  }

  async function deleteTask() {
    if (!t) return
    deleting = true
    try {
      await taskStore.remove(taskId)
      ondelete()
    } catch (e) {
      error = String(e)
      deleting = false
    }
  }

  const triaging = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('triage:') && a.state === 'running')
  )

  const evaluating = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('eval:') && a.state === 'running')
  )

  const planningAgent = $derived(
    (agentStore.list ?? []).some((a) => a.taskId === taskId && a.name?.startsWith('plan:') && a.state === 'running')
  )

  let rejectFeedback = $state('')
  let planActionLoading = $state(false)

  async function approvePlan() {
    if (!t) return
    planActionLoading = true
    try {
      t = await taskStore.approvePlan(taskId)
    } catch (e) {
      error = String(e)
    } finally {
      planActionLoading = false
    }
  }

  async function rejectPlan() {
    if (!t) return
    planActionLoading = true
    try {
      t = await taskStore.rejectPlan(taskId, rejectFeedback.trim())
      rejectFeedback = ''
    } catch (e) {
      error = String(e)
    } finally {
      planActionLoading = false
    }
  }

  let expandedRun = $state<string | null>(null)

  const pastRuns = $derived(
    (t?.agentRuns ?? []).slice().reverse()
  )

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
    Back to tasks
  </button>

  {#if error}
    <p class="text-sm text-error-500">{error}</p>
  {/if}

  {#if t}
    <div class="flex flex-col gap-6">
      <div class="flex items-start justify-between gap-4">
        <h1 class="text-2xl font-bold">{t.title}</h1>
        <div class="flex items-center gap-2">
          <StatusBadge status={t.status} />
          {#if triaging}
            <span class="inline-flex items-center gap-1 rounded-full bg-primary-200 px-2 py-0.5 text-xs font-medium text-primary-800 dark:bg-primary-700 dark:text-primary-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-primary-500"></span>
              Triaging
            </span>
          {/if}
          {#if planningAgent}
            <span class="inline-flex items-center gap-1 rounded-full bg-tertiary-200 px-2 py-0.5 text-xs font-medium text-tertiary-800 dark:bg-tertiary-700 dark:text-tertiary-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-500"></span>
              Planning
            </span>
          {/if}
          {#if evaluating}
            <span class="inline-flex items-center gap-1 rounded-full bg-warning-200 px-2 py-0.5 text-xs font-medium text-warning-800 dark:bg-warning-700 dark:text-warning-200">
              <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-warning-500"></span>
              Evaluating
            </span>
          {/if}
          <button
            type="button"
            class="rounded bg-error-500 px-2.5 py-1 text-xs font-medium text-white hover:bg-error-600 disabled:opacity-50"
            onclick={deleteTask}
            disabled={deleting}
          >
            {deleting ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </div>

      <div class="flex flex-col gap-1">
        <span class="text-sm font-medium text-surface-500">Status</span>
        <SegmentedControl
          orientation="horizontal"
          value={t.status}
          onValueChange={(details) => { if (details.value) updateStatus(details.value) }}
        >
          <SegmentedControl.Control>
            <SegmentedControl.Indicator />
            {#each statusOptions as s}
              <SegmentedControl.Item value={s.value}>
                <SegmentedControl.ItemText>{s.label}</SegmentedControl.ItemText>
                <SegmentedControl.ItemHiddenInput />
              </SegmentedControl.Item>
            {/each}
          </SegmentedControl.Control>
        </SegmentedControl>
      </div>

      <div class="flex gap-6 text-sm">
        <div class="flex flex-col gap-1">
          <span class="font-medium text-surface-500">Agent Mode</span>
          <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{t.agentMode}</span>
        </div>

        {#if t.tags?.length}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Tags</span>
            <div class="flex gap-1">
              {#each t.tags as tag}
                <span class="rounded bg-surface-200 px-2 py-0.5 dark:bg-surface-700">{tag}</span>
              {/each}
            </div>
          </div>
        {/if}

        {#if t.allowedTools?.length}
          <div class="flex flex-col gap-1">
            <span class="font-medium text-surface-500">Allowed Tools</span>
            <div class="flex gap-1">
              {#each t.allowedTools as tool}
                <span class="rounded bg-surface-200 px-2 py-0.5 font-mono text-xs dark:bg-surface-700">{tool}</span>
              {/each}
            </div>
          </div>
        {/if}
      </div>

      {#if t.body}
        <div class="flex flex-col gap-1">
          <span class="text-sm font-medium text-surface-500">Description</span>
          <pre class="whitespace-pre-wrap rounded-lg border border-surface-300 bg-surface-100 p-4 text-sm dark:border-surface-600 dark:bg-surface-900">{t.body}</pre>
        </div>
      {/if}

      <div class="flex gap-6 text-xs text-surface-400">
        <span>Created: {formatDate(t.createdAt)}</span>
        <span>Updated: {formatDate(t.updatedAt)}</span>
      </div>

      {#if t.status === 'plan-review'}
        <div class="flex flex-col gap-3 rounded-lg border border-tertiary-300 bg-tertiary-50 p-4 dark:border-tertiary-700 dark:bg-tertiary-900/30">
          <span class="text-sm font-semibold text-tertiary-700 dark:text-tertiary-300">Plan Review</span>
          <div class="flex gap-2">
            <button
              type="button"
              class="rounded-lg bg-success-500 px-4 py-2 text-sm font-medium text-white hover:bg-success-600 disabled:opacity-50"
              onclick={approvePlan}
              disabled={planActionLoading}
            >
              Approve Plan
            </button>
            <button
              type="button"
              class="rounded-lg bg-error-500 px-4 py-2 text-sm font-medium text-white hover:bg-error-600 disabled:opacity-50"
              onclick={rejectPlan}
              disabled={planActionLoading}
            >
              Reject Plan
            </button>
          </div>
          <textarea
            class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="2"
            placeholder="Rejection feedback (optional)..."
            bind:value={rejectFeedback}
          ></textarea>
        </div>
      {/if}

      <hr class="border-surface-300 dark:border-surface-600" />

      {#if runningAgent}
        <div class="flex flex-col gap-3">
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2">
              <span class="text-sm font-medium text-surface-500">Agent</span>
              <button
                type="button"
                class="font-mono text-sm text-primary-500 hover:underline"
                onclick={() => onviewagent(runningAgent!.id)}
              >
                {runningAgent.id}
              </button>
              <span class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium
                {runningAgent.state === 'running' ? 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200' : 'bg-surface-200 text-surface-800 dark:bg-surface-700 dark:text-surface-200'}">
                {#if runningAgent.state === 'running'}
                  <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-success-500"></span>
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
          {#if runningAgent.mode === 'interactive' && runningAgent.tmuxSession}
            <TerminalView agentId={runningAgent.id} />
          {:else}
            <StreamOutput agentId={runningAgent.id} />
          {/if}
        </div>
      {:else}
        <div class="flex flex-col gap-3">
          <span class="text-sm font-medium text-surface-500">Run Agent</span>
          <label class="flex items-center gap-2">
            <input
              type="checkbox"
              checked={agentMode === 'headless'}
              onchange={(e) => { agentMode = e.currentTarget.checked ? 'headless' : 'interactive' }}
              class="rounded border-surface-300 dark:border-surface-600"
            />
            <span class="text-sm">Headless</span>
          </label>
          <textarea
            class="w-full resize-y rounded-lg border border-surface-300 bg-surface-50 p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
            rows="3"
            placeholder="Enter prompt for the agent..."
            bind:value={prompt}
          ></textarea>
          <button
            type="button"
            class="w-fit rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
            onclick={startAgent}
            disabled={starting || !prompt.trim()}
          >
            {starting ? 'Starting...' : 'Start agent'}
          </button>
        </div>
      {/if}

      {#if pastRuns.length > 0}
        <hr class="border-surface-300 dark:border-surface-600" />
        <div class="flex flex-col gap-3">
          <span class="text-sm font-medium text-surface-500">Agent History</span>
          {#each pastRuns as run (run.agentId)}
            <div class="rounded-lg border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-800">
              <button
                type="button"
                class="flex w-full items-center justify-between px-3 py-2 text-left text-xs"
                onclick={() => { expandedRun = expandedRun === run.agentId ? null : run.agentId }}
              >
                <div class="flex items-center gap-2">
                  <span class="font-mono text-surface-400">{run.agentId}</span>
                  <span class="rounded bg-surface-200 px-1.5 py-0.5 dark:bg-surface-700">{run.mode}</span>
                  <span class="rounded px-1.5 py-0.5 {run.state === 'stopped' ? 'bg-surface-200 dark:bg-surface-700' : 'bg-success-200 text-success-800 dark:bg-success-700 dark:text-success-200'}">
                    {run.state || 'running'}
                  </span>
                </div>
                <div class="flex items-center gap-3 text-surface-400">
                  {#if run.costUsd > 0}
                    <span>${run.costUsd.toFixed(4)}</span>
                  {/if}
                  <span>{formatDate(run.startedAt)}</span>
                  <svg class="h-4 w-4 transition-transform {expandedRun === run.agentId ? 'rotate-180' : ''}" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
                  </svg>
                </div>
              </button>
              {#if expandedRun === run.agentId && run.result}
                <div class="border-t border-surface-300 px-3 py-2 dark:border-surface-600">
                  <pre class="whitespace-pre-wrap text-xs text-surface-300">{run.result}</pre>
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
