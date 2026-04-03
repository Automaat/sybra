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

  let deleting = $state(false)

  let t = $state<task.Task | null>(null)
  let error = $state('')
  let prompt = $state('')
  let agentMode = $state('headless')
  let starting = $state(false)
  let runningAgent = $state<agent.Agent | null>(null)

  const statusOptions = [
    { value: 'todo', label: 'Todo' },
    { value: 'in-progress', label: 'In Progress' },
    { value: 'done', label: 'Done' },
    { value: 'blocked', label: 'Blocked' },
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
      agentMode = t.agentMode || 'headless'
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
          <div class="flex flex-col gap-1">
            <SegmentedControl
              orientation="horizontal"
              value={agentMode}
              onValueChange={(details) => { if (details.value) agentMode = details.value }}
            >
              <SegmentedControl.Control>
                <SegmentedControl.Indicator />
                <SegmentedControl.Item value="headless">
                  <SegmentedControl.ItemText>Headless</SegmentedControl.ItemText>
                  <SegmentedControl.ItemHiddenInput />
                </SegmentedControl.Item>
                <SegmentedControl.Item value="interactive">
                  <SegmentedControl.ItemText>Interactive</SegmentedControl.ItemText>
                  <SegmentedControl.ItemHiddenInput />
                </SegmentedControl.Item>
              </SegmentedControl.Control>
            </SegmentedControl>
          </div>
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
            {starting ? 'Starting...' : `Start ${agentMode} agent`}
          </button>
        </div>
      {/if}
    </div>
  {:else if !error}
    <p class="text-sm opacity-60">Loading...</p>
  {/if}
</div>
